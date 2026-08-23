package metrics

import (
	"os"
	"strings"
	"testing"
)

// The kernel's text is the contract, so the fixtures are real output. Parsing
// is separate from reading precisely so this runs where /proc does not exist.
const procStat = `cpu  10000 200 3000 90000 500 0 100 0 0 0
cpu0 5000 100 1500 45000 250 0 50 0 0 0
intr 12345
ctxt 67890
`

const meminfo = `MemTotal:       16384000 kB
MemFree:         1024000 kB
MemAvailable:    8192000 kB
Buffers:          128000 kB
`

const netDev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 5000000   5000    0    0    0     0          0         0  5000000    5000    0    0    0     0       0          0
  eth0: 1000000    900    0    0    0     0          0         0   250000     300    0    0    0     0       0          0
  eth1:  500000    400    0    0    0     0          0         0   125000     150    0    0    0     0       0          0
`

func TestParseCPUCountsIdleAndIOWaitAsIdle(t *testing.T) {
	total, idle, err := parseCPU(procStat)
	if err != nil {
		t.Fatalf("parseCPU: %v", err)
	}
	if total != 103800 {
		t.Fatalf("total is %v, expected 103800", total)
	}
	// A machine waiting on its disk is not a machine doing work.
	if idle != 90500 {
		t.Fatalf("idle is %v, expected idle plus iowait, 90500", idle)
	}
}

func TestParseCPURefusesForeignText(t *testing.T) {
	_, _, err := parseCPU("this is not /proc/stat\n")
	if err == nil {
		t.Fatal("a file that is not /proc/stat was accepted")
	}
}

// Counting cache as used would report every healthy Linux machine as nearly
// out of memory.
func TestParseMeminfoUsesAvailable(t *testing.T) {
	used, total, err := parseMeminfo(meminfo)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	if total != 16384000*1024 {
		t.Fatalf("total is %v, expected the kB figure in bytes", total)
	}
	if used != (16384000-8192000)*1024 {
		t.Fatalf("used is %v, expected total minus available", used)
	}
}

func TestParseMeminfoFailsWhenTheFieldsAreMissing(t *testing.T) {
	_, _, err := parseMeminfo("MemFree: 1024 kB\n")
	if err == nil {
		t.Fatal("a meminfo without MemTotal or MemAvailable was accepted")
	}
}

func TestParseNetDevSumsInterfacesAndSkipsLoopback(t *testing.T) {
	rx, tx, err := parseNetDev(netDev)
	if err != nil {
		t.Fatalf("parseNetDev: %v", err)
	}
	if rx != 1500000 {
		t.Fatalf("rx is %v, expected eth0 plus eth1 without lo", rx)
	}
	if tx != 375000 {
		t.Fatalf("tx is %v, expected eth0 plus eth1 without lo", tx)
	}
}

func TestParseNetDevFailsWhenOnlyLoopbackExists(t *testing.T) {
	_, _, err := parseNetDev("Inter-|\n face |\n    lo: 1 2 3 4 5 6 7 8 9 10\n")
	if err == nil {
		t.Fatal("a machine with only lo reported traffic")
	}
}

func TestParseLoad(t *testing.T) {
	got, err := parseLoad("0.52 0.31 0.20 1/234 5678\n")
	if err != nil {
		t.Fatalf("parseLoad: %v", err)
	}
	if got != 0.52 {
		t.Fatalf("load is %v, expected 0.52", got)
	}
}

// Field 2 may hold spaces and parentheses, which is what breaks a parser that
// counts fields from the left.
func TestParseProcStatSurvivesAnAwkwardCommand(t *testing.T) {
	fields := []string{"1234", "((my program) )", "S", "1", "1234", "1234", "0", "-1", "4194560",
		"100", "0", "200", "0",
		"700", "300", // utime, stime
		"0", "0", "20", "0", "1", "0",
		"99999",     // starttime
		"123456789", // vsize
		"512",       // rss in pages
	}
	got, err := parseProcStat(strings.Join(fields, " ") + "\n")
	if err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if got.CPUTicks != 1000 {
		t.Fatalf("cpu ticks are %v, expected utime plus stime, 1000", got.CPUTicks)
	}
	if got.RSSBytes != 512*float64(os.Getpagesize()) {
		t.Fatalf("rss is %v, expected 512 pages in bytes", got.RSSBytes)
	}
}

func TestParseProcStatFailsOnATruncatedLine(t *testing.T) {
	_, err := parseProcStat("1234 (sh) S 1 1234\n")
	if err == nil {
		t.Fatal("a truncated stat line was accepted")
	}
}
