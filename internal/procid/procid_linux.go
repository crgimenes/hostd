package procid

import (
	"os"
	"strconv"
	"strings"
)

// token reads field 22 (starttime) of /proc/<pid>/stat, the process start time
// in clock ticks since boot. The field is never rewritten during the life of
// the process, and a PID reused after a restart gets a different value.
func token(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoProcess
		}
		return "", err
	}
	// Field 2 is the executable name in parentheses and may itself contain
	// spaces and parentheses, so the fixed-position fields are only reliable
	// after the last closing parenthesis.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", ErrNoProcess
	}
	fields := strings.Fields(string(data[end+1:]))
	// fields[0] is field 3 (state), so field 22 is at index 19.
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return "", ErrNoProcess
	}
	return fields[startTimeIndex], nil
}
