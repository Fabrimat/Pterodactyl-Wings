package system

var Version = "develop"

// Features lists the fork extensions this binary implements. The Panel uses
// this to gate functionality per-feature instead of comparing versions.
var Features = []string{"borg", "orphaned-backup-delete", "folder-download"}
