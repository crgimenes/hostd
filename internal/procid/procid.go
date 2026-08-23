// Package procid produces a stable identity for a running process.
//
// A PID alone cannot identify a process across a restart of hostd: the kernel
// reuses PID numbers, so a recorded PID may belong to a stranger by the time
// hostd comes back. The identity here pairs the PID with the process start
// time, which the kernel never rewrites, so an adopted process is provably the
// same one hostd started.
package procid

import "errors"

// ErrNoProcess reports that no process with the given PID exists.
var ErrNoProcess = errors.New("procid: no such process")

// Token returns an opaque identity for pid, stable for the whole life of that
// process and different for any process that later reuses the number.
func Token(pid int) (string, error) {
	if pid <= 0 {
		return "", ErrNoProcess
	}
	return token(pid)
}

// Matches reports whether pid is alive and still the process that produced
// want. An empty want means the caller has no recorded identity, which is
// never a match: adopting on a bare PID is how a supervisor ends up
// supervising a stranger.
func Matches(pid int, want string) bool {
	if want == "" {
		return false
	}
	got, err := Token(pid)
	if err != nil {
		return false
	}
	return got == want
}
