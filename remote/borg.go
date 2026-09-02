package remote

import (
	"context"
	"fmt"
	"net/http"

	"emperror.dev/errors"

	"github.com/pterodactyl/wings/internal/borg"
)

// Sentinel errors for the documented failure responses of the borg
// configuration endpoint, so that a caller can map them onto a status code
// without having to inspect the Panel's response a second time.
var (
	// ErrBorgBackupNotFound is returned for a backup the Panel does not know.
	ErrBorgBackupNotFound = errors.Sentinel("remote: the requested backup does not exist")

	// ErrBorgBackupForbidden is returned when this node does not own the
	// server the backup belongs to.
	ErrBorgBackupForbidden = errors.Sentinel("remote: this node does not own the server for the requested backup")

	// ErrBorgBackupInvalid is returned for a backup that was not made with the
	// borg adapter.
	ErrBorgBackupInvalid = errors.Sentinel("remote: the requested backup was not created with the borg adapter")
)

// BorgConfiguration is the borg object the Panel ships with every backup
// operation. The same object arrives pushed in the body of the requests the
// Panel makes and pulled from the endpoint below for the download path, which
// the Panel never initiates.
//
// It carries the repository passphrase and, for a remote repository, an SSH
// private key. Neither is ever persisted on this node and neither may ever be
// logged, which is why both are a borg.Secret rather than a string.
type BorgConfiguration struct {
	Repository         string      `json:"repository"`
	Archive            string      `json:"archive"`
	Passphrase         borg.Secret `json:"passphrase"`
	Encryption         string      `json:"encryption"`
	Compression        string      `json:"compression"`
	SSHPrivateKey      borg.Secret `json:"ssh_private_key"`
	SSHKnownHosts      string      `json:"ssh_known_hosts"`
	LockWait           int         `json:"lock_wait"`
	CheckpointInterval int         `json:"checkpoint_interval"`
	UploadRatelimit    int         `json:"upload_ratelimit"`
}

// GetBackupBorgConfiguration pulls the borg object for a backup. This is the
// direct analogue of the endpoint that hands S3 credentials to the node, and it
// exists for the download path only: a download is served in response to the
// user's browser hitting the JWT signed endpoint rather than to a request the
// Panel makes, and the JWT travels as a query parameter so it can never carry
// a passphrase.
func (c *client) GetBackupBorgConfiguration(ctx context.Context, backup string) (*BorgConfiguration, error) {
	res, err := c.Get(ctx, fmt.Sprintf("/backups/%s/borg", backup), nil)
	if err != nil {
		if rerr := AsRequestError(err); rerr != nil {
			switch rerr.StatusCode() {
			case http.StatusNotFound:
				return nil, errors.WithStack(ErrBorgBackupNotFound)
			case http.StatusForbidden:
				return nil, errors.WithStack(ErrBorgBackupForbidden)
			case http.StatusBadRequest:
				return nil, errors.WithStack(ErrBorgBackupInvalid)
			}
		}
		return nil, err
	}
	defer res.Body.Close()

	var data BorgConfiguration
	if err := res.BindJSON(&data); err != nil {
		return nil, err
	}
	return &data, nil
}
