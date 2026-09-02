package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/juju/ratelimit"
	"github.com/mholt/archives"
	"golang.org/x/sync/errgroup"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/borg"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
)

// BorgBackupAdapter is the adapter name reported for backups that live in a
// borg repository.
const BorgBackupAdapter AdapterType = "borg"

// BorgChecksumType is the checksum type the Panel expects for this adapter.
// The archive id borg reports is itself a content hash, so it doubles as the
// checksum.
const BorgChecksumType = "borg-archive-id"

// defaultBorgEncryption is used when the configuration carries no mode. borg
// init requires one and this is the contract's default.
const defaultBorgEncryption = "repokey-blake2"

const (
	// borgDeleteTimeout bounds a delete that runs outside of any request.
	borgDeleteTimeout = time.Hour

	// borgCompactTimeout bounds a compaction, which on a large repository is a
	// matter of minutes rather than seconds.
	borgCompactTimeout = time.Hour * 2
)

// repositoryLocks serialises repository creation per repository inside this
// process, which is the case that actually happens: two concurrent backups of
// the same server. Nothing here tries to lock across nodes, because a server
// only ever lives on one of them and the Panel does not dispatch backups
// during a transfer. The map only grows with the number of servers on the
// node, the same order as everything else wings holds in memory.
var repositoryLocks sync.Map

// Compaction is coalesced per repository. A caller always marks the repository
// dirty and only starts a worker when there is not one running already; the
// worker clears the flag before each pass and checks it again afterwards, so a
// request that arrived while it was working gets a pass of its own. The second
// pass is what matters: without it the last delete of a burst can land in the
// window between a compaction finishing and its state being cleared, and since
// no further delete is coming for that repository the space every delete in
// the burst freed would stay claimed for good.
var (
	compactMu    sync.Mutex
	compactState = make(map[string]*compactEntry)
)

type compactEntry struct {
	running bool
	dirty   bool
}

type BorgBackup struct {
	Backup

	cfg    *remote.BorgConfiguration
	runner *borg.Runner
}

var _ BackupInterface = (*BorgBackup)(nil)

func NewBorg(client remote.Client, uuid string, ignore string, cfg *remote.BorgConfiguration) *BorgBackup {
	if cfg == nil {
		// Leave the missing configuration to validate, which reports it as an
		// error rather than panicking somewhere further along.
		cfg = &remote.BorgConfiguration{}
	}
	return &BorgBackup{
		Backup: Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: BorgBackupAdapter,
		},
		cfg: cfg,
		runner: borg.NewRunner(config.Get().System.RootDirectory, borg.Repository{
			Path:       cfg.Repository,
			Passphrase: cfg.Passphrase,
			SSHKey:     cfg.SSHPrivateKey,
			KnownHosts: cfg.SSHKnownHosts,
		}),
	}
}

// WithLogContext attaches additional context to the log output for this backup.
func (b *BorgBackup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// Path returns an empty string. A borg backup is never written to this node's
// disk: the archive only ever exists inside the repository, so there is no
// local path for anything to open, and nothing in the borg flow asks for one.
func (b *BorgBackup) Path() string {
	return ""
}

// Checksum is not available without talking to the repository. The checksum
// this adapter reports is the archive id, which Details reads out of borg; the
// embedded implementation would hash a local file that never exists here.
func (b *BorgBackup) Checksum() ([]byte, error) {
	return nil, errors.New("backup: a borg backup has no local checksum, use Details instead")
}

// Size is not available without talking to the repository, for the same reason
// Checksum is not.
func (b *BorgBackup) Size() (int64, error) {
	return 0, errors.New("backup: a borg backup has no local size, use Details instead")
}

// Remove deletes the archive from the repository. The server calls this when it
// fails to notify the Panel of a successful backup, so it has to get rid of the
// archive rather than a local file that was never written. The context is built
// here because the interface does not carry one and the caller's own may
// already be finished.
func (b *BorgBackup) Remove() error {
	ctx, cancel := context.WithTimeout(context.Background(), borgDeleteTimeout)
	defer cancel()
	return b.DeleteArchive(ctx)
}

// Details returns the archive id and the original uncompressed size of the
// archive as the Panel wants them reported. The embedded implementation stats a
// local file and hardcodes a sha1 checksum, neither of which applies here.
func (b *BorgBackup) Details(ctx context.Context, _ []remote.BackupPart) (*ArchiveDetails, error) {
	info, err := b.archiveInfo(ctx)
	if err != nil {
		return nil, err
	}
	return &ArchiveDetails{
		Checksum:     info.ID,
		ChecksumType: BorgChecksumType,
		Size:         info.OriginalSize,
	}, nil
}

// Generate writes a new archive into the server's repository.
//
// Wings builds the tar itself and pipes it to borg rather than pointing borg at
// the server's data directory. That keeps the ignore patterns on the exact
// go-gitignore path the other adapters use instead of an approximation in
// borg's own pattern syntax, and it means borg never opens a path inside a tree
// the user can write to, which closes off swapping a directory for a symlink
// between the moment borg walks it and the moment it reads it.
func (b *BorgBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := b.validate(); err != nil {
		return nil, err
	}
	if err := borg.RequireVersion(ctx); err != nil {
		return nil, err
	}
	if err := b.ensureRepository(ctx); err != nil {
		return nil, err
	}

	b.log().Info("creating backup for server")

	pr, pw := io.Pipe()
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		// The stream is an uncompressed tar on purpose. Borg chunks and
		// compresses it itself, and chunking gzip output instead would give up
		// the deduplication that is the whole reason to use this adapter.
		err := (&filesystem.Archive{Filesystem: fsys, Ignore: ignore}).StreamTar(gctx, pw)
		// The tar writer closes cleanly even after a failed walk, so borg can
		// read a well formed short archive and exit 0 without noticing anything
		// is wrong. Returning the error is what keeps a truncated archive out of
		// the repository: it fails g.Wait below, which removes what was written.
		_ = pw.CloseWithError(err)
		return err
	})
	g.Go(func() error {
		res, err := b.runner.Run(gctx, borg.Cmd{
			Sub:         "import-tar",
			Common:      b.common(),
			Options:     b.importOptions(),
			Positionals: []string{b.target(), "-"},
			Stdin:       pr,
		})
		// Unblock the walker if borg gave up before reading the whole stream.
		_ = pr.CloseWithError(err)
		if err != nil {
			return err
		}
		if res.Status == borg.ExitWarning {
			b.log().WithField("warning", res.Stderr).Warn("borg reported a warning while writing the archive")
		}
		return nil
	})
	if err := g.Wait(); err != nil {
		// import-tar commits whatever it managed to read, so a failure part way
		// through can leave a short archive behind that a restore would happily
		// hand back as a whole server.
		b.removeIncompleteArchive()
		return nil, errors.WrapIf(err, "backup: failed to write the borg archive")
	}

	ad, err := b.Details(ctx, nil)
	if err != nil {
		// The Panel's completion endpoint rejects a successful backup without
		// both a checksum and a checksum type, so an archive we cannot describe
		// is one it would never record: an orphan taking up space forever.
		b.removeIncompleteArchive()
		return nil, errors.WrapIf(err, "backup: failed to read the details of the borg archive")
	}
	b.log().Info("created backup successfully")
	return ad, nil
}

// Restore extracts the archive back onto the disk. The reader is ignored: the
// source is the repository, and borg export-tar hands us a plain tar rather
// than the gzipped one the other adapters read.
func (b *BorgBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	if err := b.validate(); err != nil {
		return err
	}
	if err := borg.RequireVersion(ctx); err != nil {
		return err
	}

	pr, pw := io.Pipe()
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		err := b.exportTar(gctx, pw)
		_ = pw.CloseWithError(err)
		return err
	})
	g.Go(func() error {
		var reader io.Reader = pr
		// Steal the logic we use for making backups which will be applied when restoring
		// this specific backup. This allows us to prevent overloading the disk unintentionally.
		if writeLimit := int64(config.Get().System.Backups.WriteLimit * 1024 * 1024); writeLimit > 0 {
			reader = ratelimit.Reader(pr, ratelimit.NewBucketWithRate(float64(writeLimit), writeLimit))
		}
		// Not the package level format, which expects a gzip container.
		err := (archives.Tar{}).Extract(gctx, reader, func(ctx context.Context, f archives.FileInfo) error {
			r, err := f.Open()
			if err != nil {
				return err
			}
			defer r.Close()

			return callback(f.NameInArchive, f.FileInfo, r)
		})
		// A tar stream ends with a two block end-of-archive marker and is then
		// padded to borg's block factor. The tar reader stops right after the
		// marker and never reads that padding, so closing the pipe as soon as
		// extraction succeeds would leave export-tar still writing it into a
		// read end that is already gone, failing a restore that already copied
		// every byte correctly. DrainOnSuccess lets export-tar reach EOF on its
		// own and exit cleanly before the pipe is closed.
		err = borg.DrainOnSuccess(err, pr)
		// Unblock borg if the extraction, or the drain above, ended in an error.
		_ = pr.CloseWithError(err)
		return err
	})
	return g.Wait()
}

// ExportTar streams the archive to the writer as an ordinary tar file, which is
// what the download endpoint hands to the user's browser.
func (b *BorgBackup) ExportTar(ctx context.Context, w io.Writer) error {
	if err := b.validate(); err != nil {
		return err
	}
	if err := borg.RequireVersion(ctx); err != nil {
		return err
	}
	return b.exportTar(ctx, w)
}

// ArchiveExists reports whether the repository and this backup's archive are
// both there. A caller about to empty a server's directory to restore into it
// should ask first, so that a missing archive does not cost the server its
// files for nothing.
func (b *BorgBackup) ArchiveExists(ctx context.Context) (bool, error) {
	if err := b.validate(); err != nil {
		return false, err
	}
	if err := borg.RequireVersion(ctx); err != nil {
		return false, err
	}
	if _, err := b.archiveInfo(ctx); err != nil {
		if borg.IsNotFoundError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DeleteArchive removes this backup's archive from the repository and queues the
// compaction that reclaims the space it was using. It runs synchronously; the
// caller decides whether to wait for it.
func (b *BorgBackup) DeleteArchive(ctx context.Context) error {
	if err := b.validate(); err != nil {
		return err
	}
	if err := borg.RequireVersion(ctx); err != nil {
		return err
	}
	if _, err := b.runner.Run(ctx, borg.Cmd{
		Sub:         "delete",
		Common:      b.common(),
		Positionals: []string{b.target()},
	}); err != nil {
		// An archive that is already gone is the outcome we wanted, the same
		// way a missing file is when removing a local backup.
		if borg.IsNotFoundError(err) {
			return nil
		}
		// By the time the Panel asks for this it has already dropped its own
		// record, so there is no retry channel and a quiet failure is a storage
		// leak nobody will ever go looking for.
		b.log().WithField("error", err).Error("backup: failed to delete the borg archive, it is left behind in the repository")
		return err
	}
	b.scheduleCompact()
	return nil
}

// exportTar runs the export without revalidating, for callers that already have.
func (b *BorgBackup) exportTar(ctx context.Context, w io.Writer) error {
	// With "-" as the destination and no filename to infer from, borg writes a
	// plain tar, so there is no tar filter to undo on this side.
	_, err := b.runner.Run(ctx, borg.Cmd{
		Sub:         "export-tar",
		Common:      b.common(),
		Positionals: []string{b.target(), "-"},
		Stdout:      w,
	})
	return err
}

// archiveInfo reads the archive id and original size out of the repository.
// This comes from borg info rather than from what import-tar reports about its
// own work, since info is the call whose output we can rely on.
func (b *BorgBackup) archiveInfo(ctx context.Context) (borg.ArchiveInfo, error) {
	res, err := b.runner.Run(ctx, borg.Cmd{
		Sub:         "info",
		Common:      b.common(),
		Options:     []string{"--json"},
		Positionals: []string{b.target()},
	})
	if err != nil {
		return borg.ArchiveInfo{}, err
	}
	return borg.ParseArchiveInfo(res.Stdout)
}

// ensureRepository creates the server's repository if it is not there yet. Two
// concurrent backups of the same server both reach this, which the Panel's own
// creation throttle does not prevent, so the loser has to treat borg's refusal
// to reinitialise as a success.
func (b *BorgBackup) ensureRepository(ctx context.Context) error {
	mu := repositoryLock(b.cfg.Repository)
	mu.Lock()
	defer mu.Unlock()

	if b.repositoryUsable(ctx) {
		return nil
	}
	if borg.IsLocalRepository(b.cfg.Repository) {
		// borg does not create the directories above the repository itself.
		if err := os.MkdirAll(filepath.Dir(b.cfg.Repository), 0o700); err != nil {
			return errors.Wrap(err, "backup: could not create the parent directory of the borg repository")
		}
	}

	b.log().Info("initializing borg repository for server")
	if _, err := b.runner.Run(ctx, borg.Cmd{
		Sub:           "init",
		Common:        b.common(),
		Options:       []string{"--encryption", b.encryption()},
		Positionals:   []string{b.cfg.Repository},
		NewPassphrase: true,
	}); err != nil {
		if !borg.IsRepositoryExistsError(err) {
			return errors.WrapIf(err, "backup: failed to initialize the borg repository")
		}
		// An interrupted init can leave a directory behind that exists without
		// holding a usable repository. That reports "already exists" here and
		// then fails every archive afterwards for no visible reason, so check
		// again and say what is actually wrong.
		if !b.repositoryUsable(ctx) {
			return errors.New("backup: the borg repository path exists but is not a usable borg repository")
		}
	}
	return nil
}

// repositoryUsable reports whether the repository can be opened. A lock
// timeout lands here as a plain failure, which fails the backup rather than
// retrying forever or hanging.
func (b *BorgBackup) repositoryUsable(ctx context.Context) bool {
	_, err := b.runner.Run(ctx, borg.Cmd{
		Sub:         "info",
		Common:      b.common(),
		Options:     []string{"--json"},
		Positionals: []string{b.cfg.Repository},
	})
	return err == nil
}

// removeIncompleteArchive gets rid of an archive that cannot be reported to the
// Panel. It cannot use the context of the backup that just failed, since that
// context being finished is often the reason it failed, and it is best effort
// by necessity: there is nothing left to try if it does not work, so it is
// logged loudly instead.
func (b *BorgBackup) removeIncompleteArchive() {
	ctx, cancel := context.WithTimeout(context.Background(), borgDeleteTimeout)
	defer cancel()
	if err := b.DeleteArchive(ctx); err != nil {
		b.log().WithField("error", err).Error("backup: failed to delete the incomplete borg archive, it is left behind in the repository")
	}
}

// scheduleCompact runs a compaction for this repository in the background.
// Compaction is what actually frees the space a deleted archive was using and
// it takes minutes on a large repository, longer than the Panel is willing to
// wait on the delete call. It runs from here rather than from a cron because a
// cron would need the passphrase or the SSH key at rest on this node, which is
// exactly what shipping them per operation avoids.
func (b *BorgBackup) scheduleCompact() {
	repository, common, l := b.cfg.Repository, b.common(), b.log()
	runner := b.runner

	compactMu.Lock()
	e := compactState[repository]
	if e == nil {
		e = &compactEntry{}
		compactState[repository] = e
	}
	e.dirty = true
	if e.running {
		compactMu.Unlock()
		return
	}
	e.running = true
	compactMu.Unlock()

	go func() {
		for {
			compactMu.Lock()
			e.dirty = false
			compactMu.Unlock()

			// Deliberately not a request context: that one is finished the
			// moment the Panel's delete call is answered.
			ctx, cancel := context.WithTimeout(context.Background(), borgCompactTimeout)
			_, err := runner.Run(ctx, borg.Cmd{
				Sub:         "compact",
				Common:      common,
				Positionals: []string{repository},
			})
			cancel()
			if err != nil {
				l.WithField("error", err).Error("backup: borg compact failed, the space the deleted archive used stays claimed until the next delete")
			}

			compactMu.Lock()
			if !e.dirty {
				e.running = false
				delete(compactState, repository)
				compactMu.Unlock()
				return
			}
			compactMu.Unlock()
		}
	}()
}

// validate checks the two fields the adapter cannot do anything without. Every
// other field has a usable zero value.
func (b *BorgBackup) validate() error {
	if err := b.validateIdentifier(); err != nil {
		return err
	}
	if b.cfg.Repository == "" || b.cfg.Archive == "" {
		return errors.New("backup: the borg configuration is missing the repository or the archive name")
	}
	return nil
}

// target is the "repository::archive" positional that every archive level
// subcommand takes.
func (b *BorgBackup) target() string {
	return b.cfg.Repository + "::" + b.cfg.Archive
}

func (b *BorgBackup) common() borg.CommonOptions {
	return borg.CommonOptions{
		LockWait:        b.cfg.LockWait,
		UploadRatelimit: b.cfg.UploadRatelimit,
	}
}

func (b *BorgBackup) importOptions() []string {
	var opts []string
	if b.cfg.Compression != "" {
		opts = append(opts, "--compression", b.cfg.Compression)
	}
	if b.cfg.CheckpointInterval > 0 {
		opts = append(opts, "--checkpoint-interval", strconv.Itoa(b.cfg.CheckpointInterval))
	}
	return opts
}

func (b *BorgBackup) encryption() string {
	if b.cfg.Encryption == "" {
		return defaultBorgEncryption
	}
	return b.cfg.Encryption
}

func repositoryLock(repository string) *sync.Mutex {
	v, _ := repositoryLocks.LoadOrStore(repository, &sync.Mutex{})
	return v.(*sync.Mutex)
}
