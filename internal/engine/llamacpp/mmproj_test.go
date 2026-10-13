package llamacpp

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/engine"
)

var _ engine.ImageReader = (*Engine)(nil)

// ggufFile writes a GGUF header that carries only an architecture.
func ggufFile(t *testing.T, name, arch string) string {
	t.Helper()
	var b bytes.Buffer
	str := func(s string) {
		binary.Write(&b, binary.LittleEndian, uint64(len(s)))
		b.WriteString(s)
	}
	b.WriteString("GGUF")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	binary.Write(&b, binary.LittleEndian, uint64(0)) // tensors
	binary.Write(&b, binary.LittleEndian, uint64(1)) // key-value pairs
	str("general.architecture")
	binary.Write(&b, binary.LittleEndian, uint32(8)) // string
	str(arch)
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func mmprojModel(t *testing.T, mmproj, args string) config.Model {
	t.Helper()
	y := "  - name: m\n    engine: llamacpp\n    path: /models/m.gguf\n    gpus: [0]\n"
	if args != "" {
		y += "    args: " + args + "\n"
	}
	if mmproj != "" {
		y += "    llamacpp:\n      mmproj: " + mmproj + "\n"
	}
	return parseModel(t, y)
}

func TestMmprojBecomesAFlag(t *testing.T) {
	proj := ggufFile(t, "mmproj.gguf", "clip")
	m := mmprojModel(t, proj, `["--temp", "0"]`)
	if err := New().ValidateOptions("models[0]", config.ModelSpec(m)); err != nil {
		t.Fatal(err)
	}
	args := command(t, testEngine(8, 64*gib, gib, nil), m, 0).Args
	got, ok := flagValue(args, "--mmproj")
	if !ok || got != proj {
		t.Fatalf("--mmproj = %q, %v; want %q in %v", got, ok, proj, args)
	}
	// The operator's args still come last.
	if args[len(args)-2] != "--temp" {
		t.Fatalf("operator args are not last: %v", args)
	}
}

func TestNoMmprojNoFlag(t *testing.T) {
	args := command(t, testEngine(8, 64*gib, gib, nil), mmprojModel(t, "", ""), 0).Args
	if _, ok := flagValue(args, "--mmproj"); ok {
		t.Fatalf("--mmproj without the key: %v", args)
	}
}

// The likely paste error: the key names the model's own weights.
func TestMmprojMustBeAProjector(t *testing.T) {
	weights := ggufFile(t, "model.gguf", "llama")
	err := New().ValidateOptions("models[0]", config.ModelSpec(mmprojModel(t, weights, "")))
	if err == nil || !strings.Contains(err.Error(), "models[0].llamacpp.mmproj") || !strings.Contains(err.Error(), "llama") {
		t.Fatalf("err = %v, want the field and the architecture found", err)
	}
	notGGUF := filepath.Join(t.TempDir(), "x.gguf")
	os.WriteFile(notGGUF, []byte("hello"), 0o644)
	err = New().ValidateOptions("models[0]", config.ModelSpec(mmprojModel(t, notGGUF, "")))
	if err == nil || !strings.Contains(err.Error(), "models[0].llamacpp.mmproj") {
		t.Fatalf("err = %v, want the field named", err)
	}
}

func TestMmprojRejectsRelativeAndDuplicate(t *testing.T) {
	err := New().ValidateOptions("models[0]", config.ModelSpec(mmprojModel(t, "mmproj.gguf", "")))
	if err == nil || !strings.Contains(err.Error(), "models[0].llamacpp.mmproj") || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative path: err = %v", err)
	}
	proj := ggufFile(t, "mmproj.gguf", "clip")
	for _, flag := range []string{"--mmproj", "-mm", "--mmproj-url", "-mmu", "--no-mmproj"} {
		err = New().ValidateOptions("models[0]", config.ModelSpec(mmprojModel(t, proj, `["`+flag+`", "/x"]`)))
		if err == nil || !strings.Contains(err.Error(), "models[0].args") || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s beside the key: err = %v", flag, err)
		}
	}
}

// Validation also runs where the node's files cannot be seen, so a missing
// projector is refused when the backend is launched.
func TestMmprojMissingFileFailsAtLaunch(t *testing.T) {
	m := mmprojModel(t, "/nowhere/mmproj.gguf", "")
	spec := config.ModelSpec(m)
	if err := New().ValidateOptions("models[0]", spec); err != nil {
		t.Fatalf("validation must not need the file: %v", err)
	}
	spec.GPUs, spec.Port = m.BackendGPUs(0), 40001
	_, err := testEngine(8, 64*gib, gib, nil).Command(spec)
	if err == nil || !strings.Contains(err.Error(), "llamacpp.mmproj") || !strings.Contains(err.Error(), "/nowhere/mmproj.gguf") {
		t.Fatalf("err = %v, want the field and the path", err)
	}
}

func TestReadsImages(t *testing.T) {
	proj := ggufFile(t, "mmproj.gguf", "clip")
	cases := []struct {
		name, mmproj, args string
		reads, ok          bool
	}{
		{"the key", proj, "", true, true},
		{"a raw flag", "", `["--mmproj", "/models/p.gguf"]`, true, true},
		{"a raw flag with =", "", `["--mmproj=/models/p.gguf"]`, true, true},
		{"a projector url", "", `["-mmu", "https://example.invalid/p.gguf"]`, true, true},
		{"nothing", "", "", false, true},
		{"other args", "", `["--temp", "0"]`, false, true},
		{"switched off", "", `["--mmproj", "/models/p.gguf", "--no-mmproj"]`, false, true},
		{"a hub model may bring its own", "", `["-hf", "org/repo"]`, false, false},
	}
	for _, c := range cases {
		spec := config.ModelSpec(mmprojModel(t, c.mmproj, c.args))
		// No address: the answer must not need the network.
		reads, ok, err := New().ReadsImages(context.Background(), spec, "")
		if err != nil || reads != c.reads || ok != c.ok {
			t.Errorf("%s: reads %v ok %v err %v, want %v %v", c.name, reads, ok, err, c.reads, c.ok)
		}
	}
}
