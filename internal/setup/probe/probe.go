// Package probe lists a host's GPUs for the setup wizard: vendor, name,
// architecture and memory. NVIDIA through nvidia-smi; AMD through the kernel's
// KFD topology, which needs no ROCm tools (several fleet hosts have none);
// Apple Silicon through sysctl, where the "VRAM" is the Metal budget of
// unified memory.
package probe

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/janit/viiwork/v2/internal/gpu"
)

type GPU struct {
	Index  int
	Vendor gpu.Vendor
	Name   string
	Arch   string // "sm_86", "gfx906", "apple"
	VRAMMB int64
}

// GPUs lists the host's GPUs in the index order viiwork's gpus: uses. sys is
// the filesystem rooted at /sys.
func GPUs(ctx context.Context, goos string, run gpu.Runner, sys fs.FS) ([]GPU, error) {
	if goos == "darwin" {
		return apple(ctx, run)
	}
	if out, err := run(ctx, "nvidia-smi", "--query-gpu=index,name,memory.total,compute_cap", "--format=csv,noheader,nounits"); err == nil {
		return parseNVIDIA(string(out))
	}
	return amd(sys)
}

func parseNVIDIA(out string) ([]GPU, error) {
	var gpus []GPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 4 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		idx, err1 := strconv.Atoi(f[0])
		mem, err2 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("nvidia-smi: unexpected line %q", line)
		}
		gpus = append(gpus, GPU{Index: idx, Vendor: gpu.VendorNVIDIA, Name: f[1],
			Arch: "sm_" + strings.ReplaceAll(f[3], ".", ""), VRAMMB: mem})
	}
	return gpus, nil
}

const kfdNodes = "class/kfd/kfd/topology/nodes"

// amd reads the KFD topology: every node with SIMDs is a GPU, in numeric node
// order, which is ROCm's device order.
func amd(sys fs.FS) ([]GPU, error) {
	entries, err := fs.ReadDir(sys, kfdNodes)
	if err != nil {
		return nil, nil // no amdgpu driver: no AMD GPUs
	}
	var nodes []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			nodes = append(nodes, n)
		}
	}
	sort.Ints(nodes)
	var gpus []GPU
	for _, n := range nodes {
		dir := path.Join(kfdNodes, strconv.Itoa(n))
		props := readProps(sys, path.Join(dir, "properties"))
		if props["simd_count"] == 0 {
			continue
		}
		var vram int64
		banks, _ := fs.ReadDir(sys, path.Join(dir, "mem_banks"))
		for _, b := range banks {
			if s := readProps(sys, path.Join(dir, "mem_banks", b.Name(), "properties"))["size_in_bytes"]; s > vram {
				vram = s
			}
		}
		arch := gfxName(props["gfx_target_version"])
		gpus = append(gpus, GPU{Index: len(gpus), Vendor: gpu.VendorAMD, Name: "AMD " + arch, Arch: arch, VRAMMB: vram >> 20})
	}
	return gpus, nil
}

// gfxName spells a KFD gfx_target_version (90006, 90010, 110000) the way
// LLVM targets do (gfx906, gfx90a, gfx1100).
func gfxName(v int64) string {
	return fmt.Sprintf("gfx%d%d%x", v/10000, v/100%100, v%100)
}

func readProps(sys fs.FS, p string) map[string]int64 {
	b, err := fs.ReadFile(sys, p)
	out := map[string]int64{}
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				out[f[0]] = v
			}
		}
	}
	return out
}

func apple(ctx context.Context, run gpu.Runner) ([]GPU, error) {
	budget, err := AppleBudgetMB(ctx, run)
	if err != nil {
		return nil, err
	}
	name := "Apple Silicon"
	if out, err := run(ctx, "sysctl", "-n", "machdep.cpu.brand_string"); err == nil && strings.TrimSpace(string(out)) != "" {
		name = strings.TrimSpace(string(out))
	}
	return []GPU{{Index: 0, Vendor: gpu.VendorApple, Name: name, Arch: "apple", VRAMMB: budget}}, nil
}

// AppleBudgetMB is the memory Metal lets the GPU wire: iogpu.wired_limit_mb
// when set, else Metal's default of about 75% of RAM.
func AppleBudgetMB(ctx context.Context, run gpu.Runner) (int64, error) {
	out, err := run(ctx, "sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	ram, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %q", out)
	}
	if out, err := run(ctx, "sysctl", "-n", "iogpu.wired_limit_mb"); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && v > 0 {
			return v, nil
		}
	}
	return (ram >> 20) * 3 / 4, nil
}
