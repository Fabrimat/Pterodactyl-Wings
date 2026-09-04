package borg

import (
	"encoding/json"

	"emperror.dev/errors"
)

// ArchiveInfo is the part of "borg info --json" the adapter needs: the archive
// ID, which is itself a content hash and so doubles as the checksum, and the
// original uncompressed logical size.
type ArchiveInfo struct {
	ID           string
	OriginalSize int64
}

// The shape of "borg info --json" is not documented, so both of the plausible
// layouts are accepted. The size is a pointer to tell an absent value apart
// from a real zero, which is what an archive of an empty server directory
// reports.
type archiveInfoEntry struct {
	ID    string `json:"id"`
	Stats struct {
		OriginalSize *int64 `json:"original_size"`
	} `json:"stats"`
}

type archiveInfoResponse struct {
	Archives []archiveInfoEntry `json:"archives"`
	Archive  *archiveInfoEntry  `json:"archive"`
}

// ParseArchiveInfo reads the archive ID and original size out of borg's JSON
// output. An archive with no ID or no size is an error rather than a zero
// value: the Panel's completion endpoint rejects a successful backup that is
// missing its checksum, which would leave the archive orphaned.
func ParseArchiveInfo(b []byte) (ArchiveInfo, error) {
	var res archiveInfoResponse
	if err := json.Unmarshal(b, &res); err != nil {
		return ArchiveInfo{}, errors.Wrap(err, "borg: could not parse the output of borg info")
	}

	candidates := res.Archives
	if res.Archive != nil {
		candidates = append(candidates, *res.Archive)
	}
	for _, c := range candidates {
		if c.ID == "" || c.Stats.OriginalSize == nil {
			continue
		}
		return ArchiveInfo{ID: c.ID, OriginalSize: *c.Stats.OriginalSize}, nil
	}
	return ArchiveInfo{}, errors.New("borg: borg info did not report both an archive id and an original size")
}
