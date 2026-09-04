package borg

import "testing"

func TestParseArchiveInfo(t *testing.T) {
	for _, tt := range []struct {
		name     string
		body     string
		wantID   string
		wantSize int64
		err      bool
	}{
		{
			name:     "an archives array",
			body:     `{"archives":[{"name":"b","id":"9f3c","stats":{"original_size":4096,"compressed_size":100}}],"repository":{"id":"aa"}}`,
			wantID:   "9f3c",
			wantSize: 4096,
		},
		{
			name:     "a top level archive object",
			body:     `{"archive":{"id":"9f3c","stats":{"original_size":4096}}}`,
			wantID:   "9f3c",
			wantSize: 4096,
		},
		{
			name:     "an empty server, where a zero size is a real answer",
			body:     `{"archives":[{"id":"9f3c","stats":{"original_size":0}}]}`,
			wantID:   "9f3c",
			wantSize: 0,
		},
		{name: "garbage", body: `not json at all`, err: true},
		{name: "an empty body", body: ``, err: true},
		{name: "no archive anywhere", body: `{"repository":{"id":"aa"}}`, err: true},
		{name: "an empty archives array", body: `{"archives":[]}`, err: true},
		// Reporting a success without a checksum would 422 on the panel's
		// completion endpoint and orphan the archive.
		{name: "an archive with no id", body: `{"archives":[{"stats":{"original_size":10}}]}`, err: true},
		{name: "an archive with no stats", body: `{"archives":[{"id":"9f3c"}]}`, err: true},
		{name: "stats with no size", body: `{"archives":[{"id":"9f3c","stats":{"nfiles":3}}]}`, err: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArchiveInfo([]byte(tt.body))
			if tt.err {
				if err == nil {
					t.Fatalf("ParseArchiveInfo(%q) = %+v, want an error", tt.body, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseArchiveInfo(%q) returned an error: %v", tt.body, err)
			}
			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
			if got.OriginalSize != tt.wantSize {
				t.Errorf("OriginalSize = %d, want %d", got.OriginalSize, tt.wantSize)
			}
		})
	}
}
