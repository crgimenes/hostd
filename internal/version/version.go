// Package version carries the build identity of hostd and hostctl.
package version

// Version is set at build time with -X.
var Version = "dev"

// Protocol is the control protocol version. Clients and daemons of different
// releases must be able to tell whether they can talk to each other, so this
// is bumped independently of Version.
const Protocol = 1

// Schema is the configuration schema version accepted by this build.
const Schema = 1
