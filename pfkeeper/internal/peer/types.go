// Package peer keeps the lb pair in sync: it probes the peer over gRPC,
// pushes config snapshots from the MASTER, publishes health views in both
// directions and applies snapshots on the BACKUP.
package peer

// Role is the CARP role of the local node (as reported by the carpWatcher,
// wired in M7; this package only consumes it).
type Role string

const (
	RoleMaster Role = "MASTER"
	RoleBackup Role = "BACKUP"
	RoleInit   Role = "INIT"
)

// RoleSource provides the current local role; the daemon adapts the
// carpWatcher to it.
type RoleSource interface {
	Role() string
}

// staticRole is a fixed role source (tests, single-node labs).
type staticRole string

// NewStaticRole returns a RoleSource pinned to one role.
func NewStaticRole(r string) RoleSource { return staticRole(r) }

// Role implements RoleSource.
func (s staticRole) Role() string { return string(s) }

// LocalViewKey is the composite key convention of health views carried
// through SetLocalView: "<table>/<address>".
func LocalViewKey(table, addr string) string { return table + "/" + addr }

// splitLocalViewKey inverts LocalViewKey (first "/" is the separator;
// addresses never contain "/").
func splitLocalViewKey(key string) (table, addr string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}
