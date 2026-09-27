// Package hostinfo owns the host facts viiwork reads from the operating
// system: total RAM, used RAM, and (in mesh, which cannot import internal/)
// the default route.
//
// The parsing lives in this file, untagged, and is tested on Linux against
// captured macOS output. Only the thin readers that open a file or exec a tool
// are split into meminfo_linux.go and meminfo_darwin.go, and the Linux reader
// does exactly what internal/node and internal/engine/llamacpp did before this
// package existed.
//
// Every failure path returns 0. Zero means "this node cannot say", never a
// measured zero, which is what keeps the omitempty wire fields absent rather
// than reporting a machine with no memory.
package hostinfo

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
)

// TotalRAMBytes is the machine's physical memory, or 0 if it cannot be read.
func TotalRAMBytes() int64 { return totalRAMBytes() }

// HostMemoryMB is total and used physical memory in MiB, or 0, 0 if they
// cannot be read. "Used" is what the /mesh RAM strip renders.
func HostMemoryMB() (totalMB, usedMB int64) { return hostMemoryMB() }

// parseMemInfo is the Linux reading: used is MemTotal minus MemAvailable.
func parseMemInfo(r io.Reader) (totalMB, usedMB int64) {
	var memTotal, memAvailable int64
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMemInfoKB(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMemInfoKB(line)
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}
	totalMB = memTotal / 1024
	usedMB = (memTotal - memAvailable) / 1024
	return totalMB, usedMB
}

func parseMemInfoKB(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(fields[1], 10, 64)
	return v
}

// parseMemTotalBytes takes MemTotal out of /proc/meminfo, or 0.
func parseMemTotalBytes(data []byte) int64 {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		return parseMemInfoKB(line) * 1024
	}
	return 0
}

// parseSysctlInt reads the single number `sysctl -n` prints, or 0.
func parseSysctlInt(out []byte) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// vmStatPageSizePrefix is the header vm_stat opens with:
//
//	Mach Virtual Memory Statistics: (page size of 16384 bytes)
//
// The size is read rather than assumed: it is 16384 on Apple Silicon and 4096
// on Intel, and getting it wrong scales every figure by four.
const vmStatPageSizePrefix = "page size of "

func parseVMStatPageSize(out []byte) int64 {
	line, _, _ := strings.Cut(string(out), "\n")
	_, rest, ok := strings.Cut(line, vmStatPageSizePrefix)
	if !ok {
		return 0
	}
	size, _, _ := strings.Cut(rest, " ")
	v, err := strconv.ParseInt(size, 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// vmStatAvailable are the page counters that together make up memory the
// system can hand out without evicting anything a process is using. Wired and
// active pages are deliberately absent, and compressed pages are not free.
var vmStatAvailable = []string{
	"Pages free",
	"Pages inactive",
	"Pages speculative",
	"Pages purgeable",
}

// parseVMStat turns vm_stat output into used MiB against a known total. It
// returns 0 unless the page size, the total and at least one counter are all
// available, so a truncated or unrecognised document reads as "cannot say"
// rather than as a machine with all of its memory in use.
func parseVMStat(out []byte, totalBytes int64) (usedMB int64) {
	if totalBytes <= 0 {
		return 0
	}
	pageSize := parseVMStatPageSize(out)
	if pageSize <= 0 {
		return 0
	}
	var availPages int64
	var found bool
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if !contains(vmStatAvailable, name) {
			continue
		}
		v, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(value), "."), 10, 64)
		if err != nil {
			continue
		}
		availPages += v
		found = true
	}
	if !found {
		return 0
	}
	used := totalBytes - availPages*pageSize
	if used < 0 {
		return 0
	}
	return used / (1 << 20)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
