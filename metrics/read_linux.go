package metrics

import (
	"os"
	"strconv"
	"syscall"
)

// The filesystem an operator runs out of first is the one holding the
// services' data, which is the one hostd itself lives on.
const diskPath = "/var"

type systemSource struct{}

func (systemSource) host() (hostCounters, error) {
	var out hostCounters
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return out, err
	}
	out.CPUTotal, out.CPUIdle, err = parseCPU(string(stat))
	if err != nil {
		return out, err
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return out, err
	}
	out.MemoryUsed, out.MemoryTotal, err = parseMeminfo(string(meminfo))
	if err != nil {
		return out, err
	}
	netdev, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return out, err
	}
	out.NetRX, out.NetTX, err = parseNetDev(string(netdev))
	if err != nil {
		return out, err
	}
	loadavg, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out, err
	}
	out.Load1, err = parseLoad(string(loadavg))
	if err != nil {
		return out, err
	}
	out.DiskUsed, out.DiskTotal, err = disk(diskPath)
	return out, err
}

// Bavail rather than Bfree: the blocks reserved for root are not space this
// machine's services can use.
func disk(path string) (used float64, total float64, err error) {
	var fs syscall.Statfs_t
	err = syscall.Statfs(path, &fs)
	if err != nil {
		return 0, 0, err
	}
	size := float64(fs.Bsize)
	total = float64(fs.Blocks) * size
	return total - float64(fs.Bavail)*size, total, nil
}

func (systemSource) process(pid int) (procCounters, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procCounters{}, err
	}
	return parseProcStat(string(stat))
}
