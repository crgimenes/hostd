//go:build !linux

package api

import "syscall"

// hostd runs on Linux; elsewhere the socket has no credentials to read and the
// audit falls back to the machine itself rather than inventing a name.
func peerUID(syscall.RawConn) (int, bool) {
	return 0, false
}
