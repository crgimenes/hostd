// Package version carries the build identity of hostd and hostctl.
package version

// Set at build time with -X.
var Version = "dev"

// Bumped independently of Version: a client and a daemon from different
// releases have to be able to tell whether they can talk to each other.
const (
	Protocol = 1
	Schema   = 1
)
