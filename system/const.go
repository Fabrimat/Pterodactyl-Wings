package system

var Version = "1.13.4-fabrimat.1"

// Features lists the fork extensions this binary implements. The Panel uses
// this to gate functionality per-feature instead of comparing versions.
var Features = []string{"borg", "orphaned-backup-delete", "folder-download"}
