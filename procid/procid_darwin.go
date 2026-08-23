package procid

import (
	"encoding/binary"
	"math"
	"strconv"
	"syscall"
	"unsafe"
)

// The stdlib exposes sysctl only for string and uint32, so the raw call is
// written out here rather than pulling in golang.org/x/sys for one struct read.
const (
	ctlKern     = 1
	kernProc    = 14
	kernProcPID = 1

	// kinfo_proc opens with struct extern_proc, which opens with the start
	// time as a struct timeval.
	timevalSize = 16
)

func token(pid int) (string, error) {
	// Truncating into the mib's int32 would ask about a different process.
	if pid > math.MaxInt32 {
		return "", ErrNoProcess
	}
	// #nosec G115 -- audited: the bound above is what makes the conversion safe
	mib := [4]int32{ctlKern, kernProc, kernProcPID, int32(pid)}
	size := uintptr(0)
	// Every pointer below refers to a local whose lifetime covers the call.
	// #nosec G103 -- audited: required to reach sysctl without a dependency
	_, _, errno := syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(len(mib)),
		0,
		uintptr(unsafe.Pointer(&size)),
		0,
		0,
	)
	if errno != 0 {
		return "", errnoError(errno)
	}
	if size < timevalSize {
		return "", ErrNoProcess
	}
	buf := make([]byte, size)
	// #nosec G103 -- audited: same sysctl call, now with a buffer to fill
	_, _, errno = syscall.Syscall6(
		syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
		0,
	)
	if errno != 0 {
		return "", errnoError(errno)
	}
	// A dead process yields a successful call with an empty record.
	if size < timevalSize {
		return "", ErrNoProcess
	}
	sec := binary.LittleEndian.Uint64(buf[0:8])
	usec := binary.LittleEndian.Uint32(buf[8:12])
	if sec == 0 && usec == 0 {
		return "", ErrNoProcess
	}
	return strconv.FormatUint(sec, 10) + "." + strconv.FormatUint(uint64(usec), 10), nil
}

func errnoError(errno syscall.Errno) error {
	if errno == syscall.ESRCH || errno == syscall.EINVAL {
		return ErrNoProcess
	}
	return errno
}
