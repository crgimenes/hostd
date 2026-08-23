package api

import (
	"syscall"
)

// The kernel's own answer to "who is on the other end", which is why an
// operator cannot claim to be somebody else.
func peerUID(raw syscall.RawConn) (int, bool) {
	var uid int
	var found bool
	err := raw.Control(func(fd uintptr) {
		creds, credErr := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if credErr != nil {
			return
		}
		uid = int(creds.Uid)
		found = true
	})
	if err != nil {
		return 0, false
	}
	return uid, found
}
