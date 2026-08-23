package procid

import (
	"os"
	"strconv"
	"strings"
)

// Field 22 of /proc/<pid>/stat is the start time in clock ticks since boot.
func token(pid int) (string, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoProcess
		}
		return "", err
	}
	// Field 2 is the executable name and may contain spaces and parentheses,
	// so positions are only reliable after the last closing one.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", ErrNoProcess
	}
	fields := strings.Fields(string(data[end+1:]))
	// fields[0] is field 3, so field 22 is at index 19.
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex {
		return "", ErrNoProcess
	}
	return fields[startTimeIndex], nil
}
