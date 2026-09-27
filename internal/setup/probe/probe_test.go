package probe

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/janit/viiwork/v2/internal/gpu"
)

func runner(outputs map[string]string) gpu.Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name
		for _, a := range args {
			key += " " + a
		}
		for k, v := range outputs {
			if k == key || k == name {
				return []byte(v), nil
			}
		}
		return nil, errors.New("not found: " + key)
	}
}

func TestNVIDIA(t *testing.T) {
	run := runner(map[string]string{"nvidia-smi": "0, NVIDIA RTX A4000, 16376, 8.6\n1, NVIDIA GeForce RTX 3080, 10240, 8.6\n"})
	got, err := GPUs(context.Background(), "linux", run, fstest.MapFS{})
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v, %v", got, err)
	}
	if got[0] != (GPU{Index: 0, Vendor: gpu.VendorNVIDIA, Name: "NVIDIA RTX A4000", Arch: "sm_86", VRAMMB: 16376}) {
		t.Errorf("gpu 0 = %+v", got[0])
	}
}

// kfd builds a KFD topology like gb1's: node 0 is the CPU, nodes 1..n the
// cards, listed by the kernel as 1, 10, 2, …
func kfd(n int, gfx string, bytes int64) fstest.MapFS {
	fsys := fstest.MapFS{
		"class/kfd/kfd/topology/nodes/0/properties":             {Data: []byte("cpu_cores_count 4\nsimd_count 0\ngfx_target_version 0\n")},
		"class/kfd/kfd/topology/nodes/0/mem_banks/0/properties": {Data: []byte("heap_type 0\nsize_in_bytes 67316551680\n")},
	}
	for i := 1; i <= n; i++ {
		fsys[fmt.Sprintf("class/kfd/kfd/topology/nodes/%d/properties", i)] = &fstest.MapFile{Data: []byte(fmt.Sprintf(
			"cpu_cores_count 0\nsimd_count 240\ngfx_target_version %s\nlocation_id %d\n", gfx, 2560+i))}
		fsys[fmt.Sprintf("class/kfd/kfd/topology/nodes/%d/mem_banks/0/properties", i)] = &fstest.MapFile{Data: []byte(fmt.Sprintf(
			"heap_type 2\nsize_in_bytes %d\n", bytes))}
	}
	return fsys
}

func TestAMDFromKFD(t *testing.T) {
	got, err := GPUs(context.Background(), "linux", runner(nil), kfd(10, "90006", 17163091968))
	if err != nil || len(got) != 10 {
		t.Fatalf("%d gpus, %v", len(got), err)
	}
	for i, g := range got {
		if g.Index != i || g.Vendor != gpu.VendorAMD || g.Arch != "gfx906" || g.VRAMMB != 16368 {
			t.Errorf("gpu %d = %+v", i, g)
		}
	}
	// Numeric node order: the card at node 10 is index 9, not index 1.
	fsys := kfd(10, "90006", 17163091968)
	fsys["class/kfd/kfd/topology/nodes/10/properties"].Data = []byte("simd_count 240\ngfx_target_version 90010\n")
	got, _ = GPUs(context.Background(), "linux", runner(nil), fsys)
	if got[9].Arch != "gfx90a" || got[1].Arch != "gfx906" {
		t.Errorf("order: index 1 %s, index 9 %s", got[1].Arch, got[9].Arch)
	}
	rdna, _ := GPUs(context.Background(), "linux", runner(nil), kfd(1, "110000", 24<<30))
	if rdna[0].Arch != "gfx1100" {
		t.Errorf("gfx1100: %+v", rdna[0])
	}
}

func TestNoGPUs(t *testing.T) {
	got, err := GPUs(context.Background(), "linux", runner(nil), fstest.MapFS{})
	if err != nil || len(got) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

func TestApple(t *testing.T) {
	run := runner(map[string]string{
		"sysctl -n hw.memsize":               "38654705664\n",
		"sysctl -n iogpu.wired_limit_mb":     "0\n",
		"sysctl -n machdep.cpu.brand_string": "Apple M3 Max\n",
	})
	got, err := GPUs(context.Background(), "darwin", run, fstest.MapFS{})
	if err != nil || len(got) != 1 || got[0].Vendor != gpu.VendorApple || got[0].Name != "Apple M3 Max" {
		t.Fatalf("%+v, %v", got, err)
	}
	// wired limit 0 means Metal's default: about 75% of RAM.
	if got[0].VRAMMB != 36864*3/4 {
		t.Errorf("budget %d MiB", got[0].VRAMMB)
	}
	run = runner(map[string]string{
		"sysctl -n hw.memsize":               "38654705664\n",
		"sysctl -n iogpu.wired_limit_mb":     "30000\n",
		"sysctl -n machdep.cpu.brand_string": "Apple M3 Max\n",
	})
	if b, _ := AppleBudgetMB(context.Background(), run); b != 30000 {
		t.Errorf("explicit wired limit: %d", b)
	}
}
