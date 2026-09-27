package hostinfo

import "os"

// totalRAMBytes returns MemTotal from /proc/meminfo, or 0 if unreadable. This
// is verbatim what internal/engine/llamacpp did for the --no-mmap rule.
func totalRAMBytes() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemTotalBytes(data)
}

// hostMemoryMB is the v1 proxy's reading of /proc/meminfo: used is total minus
// MemAvailable, the figure the RAM strip on /mesh renders.
func hostMemoryMB() (totalMB, usedMB int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	return parseMemInfo(f)
}
