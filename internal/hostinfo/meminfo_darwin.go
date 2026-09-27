package hostinfo

import (
	"context"
	"os/exec"
	"time"
)

// readTimeout bounds the two tools below. Both answer in milliseconds; the
// bound exists so a wedged exec cannot stall a status build.
const readTimeout = 2 * time.Second

func run(name string, args ...string) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil
	}
	return out
}

// totalRAMBytes returns hw.memsize, the machine's physical memory, or 0.
func totalRAMBytes() int64 { return parseSysctlInt(run("sysctl", "-n", "hw.memsize")) }

// hostMemoryMB reports used memory as total minus what vm_stat says the system
// can still hand out. There is no MemAvailable on darwin, so the free,
// inactive, speculative and purgeable counters stand in for it.
func hostMemoryMB() (totalMB, usedMB int64) {
	total := totalRAMBytes()
	if total <= 0 {
		return 0, 0
	}
	return total / (1 << 20), parseVMStat(run("vm_stat"), total)
}
