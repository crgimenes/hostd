// Package procid produces a stable identity for a running process.
//
// The kernel reuses PID numbers, so a recorded PID may belong to a stranger by
// the time hostd comes back. Pairing it with the process start time, which the
// kernel never rewrites, is what makes adoption provable.
package procid

import "errors"

var ErrNoProcess = errors.New("procid: no such process")

// Stable for that process's life, and different for any process that later
// reuses the number.
func Token(pid int) (string, error) {
	if pid <= 0 {
		return "", ErrNoProcess
	}
	return token(pid)
}

// An empty want never matches: adopting on a bare PID is how a supervisor ends
// up supervising a stranger.
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
