// Package version carries the build identity of hostd and hostctl.
package version

// Set at build time with -X. The house convention stamps main.Version, the
// one package name every project shares, so each command hands it here rather
// than every reader importing main.
var Version = "dev"

// A build that was not stamped keeps saying "dev", which is true.
func Set(stamped string) {
	if stamped == "" {
		return
	}
	Version = stamped
}

// Bumped independently of Version: a client and a daemon from different
// releases have to be able to tell whether they can talk to each other.
const (
	Protocol = 1
	Schema   = 1
)
