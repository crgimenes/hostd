// Package daemon carries the hostd binaries a release of hostctl installs.
//
// The pairing is the point: hostctl at some version installs hostd at that
// same version, atomically, offline, because the daemon travels inside the
// client. There is no version to choose, no download, no cache and no skew —
// putting an older daemon on a machine is done with the older hostctl, which
// the releases page keeps.
package daemon

import (
	"embed"
	"fmt"
	"strings"
)

// The zips land here during `make dist`, right before hostctl is built, so a
// released hostctl carries them. A development build carries only the
// placeholder, and Zip says so instead of failing at some machine's far end.
//
//go:embed zips
var zips embed.FS

// Zip answers with the hostd release archive for one architecture, the same
// artifact the GitHub release publishes separately.
func Zip(arch string) ([]byte, error) {
	content, err := zips.ReadFile("zips/hostd-linux-" + arch + ".zip")
	if err != nil {
		return nil, fmt.Errorf("this hostctl was built without an embedded hostd for linux/%s; release builds carry it (make dist), development builds do not", arch)
	}
	return content, nil
}

// Version is the tag the carried daemon was built from, read from beside the
// zips rather than assumed: in a release build it always equals hostctl's own
// version, but a development tree can hold zips an earlier `make dist` left,
// and reporting hostctl's version for THOSE would claim a pairing that is not
// there.
func Version() string {
	content, err := zips.ReadFile("zips/VERSION")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

// Carried lists the architectures this build has a daemon for, empty on a
// development build. It is how `hostctl -version` tells a release build from
// one somebody compiled in a working tree.
func Carried() []string {
	entries, err := zips.ReadDir("zips")
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "hostd-linux-") && strings.HasSuffix(name, ".zip") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(name, "hostd-linux-"), ".zip"))
		}
	}
	return out
}
