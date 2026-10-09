package hostinfo

import (
	"os"
	"strings"
	"testing"
)

// TestParseMemInfo is the Linux reading moved from internal/node: used is
// MemTotal minus MemAvailable, in MiB.
func TestParseMemInfo(t *testing.T) {
	meminfo := "MemTotal:       65536000 kB\nMemFree:         1000000 kB\nMemAvailable:   16384000 kB\n"
	if total, used := parseMemInfo(strings.NewReader(meminfo)); total != 64000 || used != 48000 {
		t.Errorf("total=%d used=%d, want 64000/48000", total, used)
	}
}

func TestParseMemInfoUnreadable(t *testing.T) {
	// 0 means "cannot say", never a measured zero.
	if total, used := parseMemInfo(strings.NewReader("nothing useful\n")); total != 0 || used != 0 {
		t.Errorf("total=%d used=%d, want 0/0", total, used)
	}
}

func TestParseMemTotalKB(t *testing.T) {
	const line = "MemTotal:       65536000 kB\nMemFree: 1 kB\n"
	if got := parseMemTotalBytes([]byte(line)); got != 65536000*1024 {
		t.Errorf("parseMemTotalBytes = %d, want %d", got, int64(65536000)*1024)
	}
	if got := parseMemTotalBytes([]byte("MemFree: 1 kB\n")); got != 0 {
		t.Errorf("no MemTotal: got %d, want 0", got)
	}
}

// The page size on Apple Silicon is 16 KiB, not the 4 KiB an x86 Linux habit
// would assume, so it is read from the header rather than hardcoded.
func TestParseVMStatPageSize(t *testing.T) {
	out := readFixture(t, "vm_stat-darwin.txt")
	if got := parseVMStatPageSize(out); got != 16384 {
		t.Errorf("page size = %d, want 16384", got)
	}
	if got := parseVMStatPageSize([]byte("Mach Virtual Memory Statistics:\nPages free: 1.\n")); got != 0 {
		t.Errorf("header without a page size: got %d, want 0", got)
	}
}

func TestParseVMStat(t *testing.T) {
	const total = 38654705664 // hw.memsize on the M3 Max the fixture came from

	// free 3993 + inactive 426526 + speculative 2124 + purgeable 27482
	const availPages = 3993 + 426526 + 2124 + 27482
	const wantAvail = availPages * 16384
	const wantUsedMB = (total - wantAvail) / (1 << 20)

	out := readFixture(t, "vm_stat-darwin.txt")
	usedMB := parseVMStat(out, total)
	if usedMB != wantUsedMB {
		t.Errorf("usedMB = %d, want %d", usedMB, wantUsedMB)
	}
	if usedMB <= 0 || usedMB >= total/(1<<20) {
		t.Errorf("usedMB = %d is not inside 0..%d", usedMB, total/(1<<20))
	}
}

func TestParseVMStatCannotSay(t *testing.T) {
	out := readFixture(t, "vm_stat-darwin.txt")
	cases := []struct {
		name  string
		in    []byte
		total int64
	}{
		{"no total", out, 0},
		{"truncated", []byte("Mach Virtual Memory Statistics: (page size of 16384 bytes)\n"), 38654705664},
		{"empty", nil, 38654705664},
		{"no page size", []byte("Pages free: 100.\nPages inactive: 100.\n"), 38654705664},
	}
	for _, tc := range cases {
		if got := parseVMStat(tc.in, tc.total); got != 0 {
			t.Errorf("%s: usedMB = %d, want 0 (cannot say)", tc.name, got)
		}
	}
}

// Unknown and reordered keys must not disturb the reading: vm_stat's field list
// is not a stable contract.
func TestParseVMStatToleratesUnknownKeys(t *testing.T) {
	const total = 16 << 30
	in := []byte("Mach Virtual Memory Statistics: (page size of 4096 bytes)\n" +
		"Some future counter:                 12345.\n" +
		"Pages purgeable:                       100.\n" +
		"Pages free:                            100.\n" +
		"Pages speculative:                     100.\n" +
		"Pages inactive:                        100.\n")
	const wantUsedMB = (total - 400*4096) / (1 << 20)
	if got := parseVMStat(in, total); got != wantUsedMB {
		t.Errorf("usedMB = %d, want %d", got, wantUsedMB)
	}
}

func TestParseSysctlInt(t *testing.T) {
	if got := parseSysctlInt([]byte("38654705664\n")); got != 38654705664 {
		t.Errorf("got %d", got)
	}
	for _, bad := range []string{"", "\n", "not a number\n"} {
		if got := parseSysctlInt([]byte(bad)); got != 0 {
			t.Errorf("%q: got %d, want 0", bad, got)
		}
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
