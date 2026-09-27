package setup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/janit/viiwork/v2/internal/accept"
	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/joincode"
	"github.com/janit/viiwork/v2/internal/setup/discover"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
	"github.com/janit/viiwork/v2/meshapi"
)

// writeGGUF writes a llama-like GGUF header at path, padded (sparsely) to size.
func writeGGUF(t *testing.T, path, arch string, size int64) {
	t.Helper()
	var b bytes.Buffer
	w32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	w64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { w64(uint64(len(s))); b.WriteString(s) }
	kv := func(k string, v uint32) { str(k); w32(4); w32(v) } // 4: uint32
	b.WriteString("GGUF")
	w32(3)
	w64(0)
	w64(5)
	str("general.architecture")
	w32(8) // string
	str(arch)
	kv(arch+".block_count", 32)
	kv(arch+".context_length", 32768)
	kv(arch+".attention.head_count_kv", 8)
	kv(arch+".attention.key_length", 128)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, size); err != nil {
		t.Fatal(err)
	}
}

type fake struct {
	host  Host
	out   *bytes.Buffer
	calls *[]string
	node  *httptest.Server
}

// newFake is a two-card NVIDIA host with Docker and the toolkit, a models
// directory, and a node that is healthy as soon as it is asked.
func newFake(t *testing.T) *fake {
	root := t.TempDir()
	models := filepath.Join(root, "models")
	writeGGUF(t, filepath.Join(models, "alpha-8b.gguf"), "llama", 7<<30)
	writeGGUF(t, filepath.Join(models, "sub", "beta-00001-of-00002.gguf"), "llama", 2<<30)
	os.WriteFile(filepath.Join(models, "sub", "beta-00002-of-00002.gguf"), make([]byte, 16), 0o644)
	writeGGUF(t, filepath.Join(models, "mmproj-alpha.gguf"), "clip", 1<<20)
	os.WriteFile(filepath.Join(models, "broken.gguf"), []byte("not a model"), 0o644)

	st := meshapi.NodeStatus{Node: "node-a", Addr: "127.0.0.1", Models: []meshapi.ModelStatus{
		{Name: "alpha", Backends: []meshapi.BackendStatus{{ID: "alpha-0", Status: meshapi.StatusHealthy}}},
		{Name: "beta", Backends: []meshapi.BackendStatus{{ID: "beta-0", Status: meshapi.StatusHealthy}}},
	}}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case meshapi.PathStatus:
			json.NewEncoder(w).Encode(st)
		case meshapi.PathCluster:
			json.NewEncoder(w).Encode(meshapi.ClusterResponse{Members: []meshapi.Member{{Node: "node-a", State: meshapi.MemberAlive, Status: &st}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(node.Close)
	exe := filepath.Join(t.TempDir(), "viiwork")
	os.WriteFile(exe, []byte("#!binary"), 0o755)

	f := &fake{out: &bytes.Buffer{}, calls: &[]string{}, node: node}
	addr := strings.TrimPrefix(node.URL, "http://")
	_, port, _ := strings.Cut(addr, ":")
	var peerPort int
	json.Unmarshal([]byte(port), &peerPort)
	f.host = Host{
		GOOS: "linux", GOARCH: "amd64", Version: "v2.6.0", Root: root, Home: "/home/u", Euid: 0,
		Hostname: func() (string, error) { return "Node-A.example", nil },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch name + " " + strings.Join(args, " ") {
			case "nvidia-smi --query-gpu=index,name,memory.total,compute_cap --format=csv,noheader,nounits":
				return []byte("0, NVIDIA RTX A4000, 16376, 8.6\n1, NVIDIA RTX A4000, 16376, 8.6\n"), nil
			case "docker compose version":
				return []byte("Docker Compose version v2.29.7\n"), nil
			case "docker version --format {{.Server.Version}}":
				return []byte("27.3.1\n"), nil
			case "docker ps -a --filter name=^viiwork$ --format {{.Names}}":
				return nil, nil
			case "docker info --format {{json .Runtimes}}":
				return []byte(`{"nvidia":{"path":"nvidia-container-runtime"},"runc":{"path":"runc"}}`), nil
			}
			return nil, errors.New("not found")
		},
		Exec: func(_ context.Context, _ io.Writer, name string, args ...string) error {
			*f.calls = append(*f.calls, name+" "+strings.Join(args, " "))
			return nil
		},
		Sys:          fstest.MapFS{},
		PortFree:     func(string, int) bool { return true },
		Discover:     func(context.Context) []discover.Node { return nil },
		Tailnet:      func(context.Context) bool { return false },
		Rand:         bytes.NewReader(bytes.Repeat([]byte{7}, 64)),
		Executable:   exe,
		HTTP:         node.Client(),
		Accept:       accept.DefaultEnv(),
		NodeAPI:      addr,
		PeerAPIPort:  peerPort,
		Out:          f.out,
		ReadyTimeout: 10 * time.Second,
		UpTimeout:    10 * time.Second,
		Poll:         20 * time.Millisecond,
	}
	return f
}

func (f *fake) run(answers ...string) error {
	return Run(context.Background(), f.host, prompt.Script(f.out, answers...), install.ConfigFile)
}

func (f *fake) path(p string) string { return filepath.Join(f.host.Root, p) }

// newMesh answers every question for a new secured mesh with updates on.
var newMesh = []string{
	"",      // models directory: /models, found under the root
	"all",   // both models
	"alpha", // name for alpha-8b.gguf
	"",      // name for the beta shard: beta
	"",      // accept the layout
	"",      // mesh: a new secured mesh (nothing was discovered)
	"y",     // accept mesh-wide updates
	"",      // node name: node-a
	"",      // write and start
}

func TestNewSecuredMesh(t *testing.T) {
	f := newFake(t)
	if err := f.run(newMesh...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if testing.Verbose() {
		t.Log("\n" + f.out.String())
	}
	env, err := os.ReadFile(f.path(install.EnvFile))
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(strings.TrimPrefix(string(env), "VIIWORK_MESH_SECRET="))
	cfg, err := config.Load(f.path(install.ConfigFile), func(k string) (string, bool) { return secret, k == "VIIWORK_MESH_SECRET" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Name != "node-a" || cfg.Mesh.Open || !cfg.Update.Enabled || cfg.Mesh.Network != config.NetworkLAN {
		t.Errorf("config %+v", cfg)
	}
	var paths []string
	for _, m := range cfg.Models {
		paths = append(paths, m.Path)
	}
	if !slices.Equal(paths, []string{"/models/alpha-8b.gguf", "/models/sub/beta-00001-of-00002.gguf"}) {
		t.Errorf("model paths %q", paths)
	}
	m, err := install.ReadManifest(f.path(install.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Files, []string{install.ConfigFile, install.EnvFile, install.ComposeFile}) ||
		m.Binary != install.BinaryPath || !slices.Equal(m.Images, []string{"ghcr.io/janit/viiwork-llamacpp-cuda:v2.6.0"}) ||
		!slices.Equal(m.ModelDirs, []string{"/models"}) || m.Compose == nil {
		t.Errorf("manifest %+v", m)
	}
	want := []string{
		"docker pull ghcr.io/janit/viiwork-llamacpp-cuda:v2.6.0",
		"docker compose -f " + f.path(install.ComposeFile) + " -p viiwork up -d",
	}
	if !slices.Equal(*f.calls, want) {
		t.Errorf("docker calls %q", *f.calls)
	}
	out := f.out.String()
	if strings.Contains(out, secret) {
		t.Error("the secret was printed")
	}
	if !strings.Contains(out, "viiwork1-") {
		t.Errorf("no join code for the next machine:\n%s", out)
	}
	if !strings.Contains(out, "skipped broken.gguf") || strings.Contains(out, "mmproj") || strings.Contains(out, "00002-of") {
		t.Errorf("model listing:\n%s", out)
	}
}

// Nothing is written until the last answer, whichever answer is missing.
func TestAbortAtEveryStepWritesNothing(t *testing.T) {
	for k := 0; k < len(newMesh); k++ {
		f := newFake(t)
		answers := slices.Clone(newMesh[:k])
		if k == len(newMesh)-1 {
			answers = append(answers, "n") // and a no at the final confirm
		}
		if err := f.run(answers...); !errors.Is(err, prompt.ErrAbort) {
			t.Fatalf("cut at %d: %v", k, err)
		}
		for _, p := range []string{"/etc", "/usr", "/var"} {
			if _, err := os.Stat(f.path(p)); err == nil {
				t.Errorf("cut at %d: %s was written", k, p)
			}
		}
		if len(*f.calls) != 0 {
			t.Errorf("cut at %d: docker was called: %q", k, *f.calls)
		}
	}
}

func TestJoinWithACode(t *testing.T) {
	f := newFake(t)
	f.host.Discover = func(context.Context) []discover.Node {
		return []discover.Node{{Name: "node-b", Version: "v2.6.0", API: "127.0.0.1:8086"}}
	}
	secret := bytes.Repeat([]byte{9}, 32)
	code, err := joincode.Encode(joincode.Code{Secret: secret, Seed: "127.0.0.1:7946"})
	if err != nil {
		t.Fatal(err)
	}
	answers := slices.Clone(newMesh)
	answers[5] = ""                                             // the default is now "join", because a node was found
	answers = slices.Insert(answers, 6, "not-a-code", "", code) // a bad code asks the mesh question again
	if err := f.run(answers...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	env, _ := os.ReadFile(f.path(install.EnvFile))
	if !strings.Contains(string(env), base64.StdEncoding.EncodeToString(secret)) {
		t.Errorf("mesh.env %q", env)
	}
	cfg, _ := config.Parse(mustRead(t, f.path(install.ConfigFile)))
	if !slices.Equal(cfg.Mesh.Seeds, []string{"127.0.0.1:7946"}) {
		t.Errorf("seeds %q", cfg.Mesh.Seeds)
	}
	if !strings.Contains(f.out.String(), "unknown join code version") {
		t.Errorf("a bad code was not explained:\n%s", f.out)
	}
	if strings.Contains(f.out.String(), "This code contains the mesh secret") {
		t.Error("a joining node printed a join code of its own")
	}
}

func TestOpenMeshSkipsUpdates(t *testing.T) {
	f := newFake(t)
	answers := slices.Clone(newMesh)
	answers[5] = "3"                         // open mesh
	answers = slices.Insert(answers, 6, "y") // confirm the warning
	answers = slices.Delete(answers, 7, 8)   // no update question
	if err := f.run(answers...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	cfg, _ := config.Parse(mustRead(t, f.path(install.ConfigFile)))
	if !cfg.Mesh.Open || cfg.Update.Enabled {
		t.Errorf("mesh %+v update %+v", cfg.Mesh, cfg.Update)
	}
	if _, err := os.Stat(f.path(install.EnvFile)); err == nil {
		t.Error("mesh.env written for an open mesh")
	}
}

func TestDevelopmentBuildWritesConfigOnly(t *testing.T) {
	f := newFake(t)
	f.host.Version = "dev"
	if err := f.run(newMesh...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if _, err := os.Stat(f.path(install.ConfigFile)); err != nil {
		t.Error("no config")
	}
	if _, err := os.Stat(f.path(install.ComposeFile)); err == nil {
		t.Error("a compose file without an image")
	}
	m, _ := install.ReadManifest(f.path(install.ManifestFile))
	if m.Compose != nil || len(m.Images) != 0 || len(*f.calls) != 0 {
		t.Errorf("manifest %+v calls %q", m, *f.calls)
	}
	if !strings.Contains(f.out.String(), "development build") {
		t.Errorf("no reason given:\n%s", f.out)
	}
}

func TestAnEditTheNodeWouldRefuseIsAskedAgain(t *testing.T) {
	f := newFake(t)
	answers := slices.Clone(newMesh)
	// Edit alpha onto beta's card: alpha, GPUs 0 1, 1 per backend, context, parallel.
	answers = slices.Insert(answers, 4, "2", "1", "0 1", "1", "8192", "2", "")
	// The accept that follows is refused, so drop beta and accept again.
	answers = slices.Insert(answers, 11, "3", "2")
	if err := f.run(answers...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !strings.Contains(f.out.String(), "The node would refuse this layout") {
		t.Errorf("the refusal was not shown:\n%s", f.out)
	}
	cfg, _ := config.Parse(mustRead(t, f.path(install.ConfigFile)))
	if len(cfg.Models) != 1 || cfg.Models[0].Name != "alpha" || !slices.Equal(cfg.Models[0].GPUs, []int{0, 1}) {
		t.Errorf("models %+v", cfg.Models)
	}
}

func TestNodeNeverReadyKeepsFilesAndExplains(t *testing.T) {
	f := newFake(t)
	f.node.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == meshapi.PathStatus {
			json.NewEncoder(w).Encode(meshapi.NodeStatus{Node: "node-a", Models: []meshapi.ModelStatus{
				{Name: "alpha", Backends: []meshapi.BackendStatus{{ID: "alpha-0", Status: "unhealthy", Phase: "loading"}}}}})
		}
	})
	f.host.ReadyTimeout = 1500 * time.Millisecond
	if err := f.run(newMesh...); err == nil {
		t.Fatal("setup reported success")
	}
	out := f.out.String()
	if !strings.Contains(out, "alpha-0: unhealthy loading") || !strings.Contains(out, "viiwork uninstall") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := os.Stat(f.path(install.ManifestFile)); err != nil {
		t.Error("the files were not kept")
	}
}

func TestPreflightRefusals(t *testing.T) {
	cases := map[string]func(f *fake){
		"root": func(f *fake) { f.host.Euid = 1000 },
		"exists": func(f *fake) {
			os.MkdirAll(f.path("/etc/viiwork"), 0o755)
			os.WriteFile(f.path(install.ConfigFile), nil, 0o644)
		},
		"uninstall": func(f *fake) {
			os.MkdirAll(f.path("/etc/viiwork"), 0o755)
			os.WriteFile(f.path(install.ManifestFile), nil, 0o644)
		},
		"Docker": func(f *fake) {
			run := f.host.Run
			f.host.Run = func(ctx context.Context, n string, a ...string) ([]byte, error) {
				if n == "docker" {
					return nil, errors.New("no")
				}
				return run(ctx, n, a...)
			}
		},
		"container named viiwork": func(f *fake) {
			run := f.host.Run
			f.host.Run = func(ctx context.Context, n string, a ...string) ([]byte, error) {
				if n == "docker" && a[0] == "ps" {
					return []byte("viiwork\n"), nil
				}
				return run(ctx, n, a...)
			}
		},
		"mesh.env": func(f *fake) {
			os.MkdirAll(f.path("/etc/viiwork"), 0o755)
			os.WriteFile(f.path(install.EnvFile), nil, 0o600)
		},
		"docker-compose.yaml": func(f *fake) {
			os.MkdirAll(f.path("/etc/viiwork"), 0o755)
			os.WriteFile(f.path(install.ComposeFile), nil, 0o644)
		},
		"Compose v2": func(f *fake) {
			run := f.host.Run
			f.host.Run = func(ctx context.Context, n string, a ...string) ([]byte, error) {
				if n == "docker" && a[0] == "compose" {
					return nil, errors.New("'compose' is not a docker command")
				}
				return run(ctx, n, a...)
			}
		},
		"7946/udp": func(f *fake) { f.host.PortFree = func(n string, p int) bool { return !(n == "udp" && p == 7946) } },
		"Container Toolkit": func(f *fake) {
			run := f.host.Run
			f.host.Run = func(ctx context.Context, n string, a ...string) ([]byte, error) {
				if n == "docker" && a[0] == "info" {
					return []byte(`{"runc":{}}`), nil
				}
				return run(ctx, n, a...)
			}
		},
	}
	for want, breakIt := range cases {
		f := newFake(t)
		breakIt(f)
		err := f.run() // no answers: a refusal comes before any question
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", want, err)
		}
		if _, err := os.Stat(f.path("/usr")); err == nil {
			t.Errorf("%s: files written", want)
		}
	}
	f := newFake(t)
	if err := Run(context.Background(), f.host, prompt.Script(io.Discard), "/tmp/elsewhere.yaml"); err == nil || !strings.Contains(err.Error(), "--config") {
		t.Errorf("--config elsewhere: %v", err)
	}
}

func TestNodeName(t *testing.T) {
	for in, want := range map[string]string{
		"Name-MacBook-Pro": "name-macbook-pro",
		"Node-A.example":   "node-a",
		"gpu_box 3":        "gpu-box-3",
		"--":               "node",
	} {
		if got := NodeName(in); got != want {
			t.Errorf("NodeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// statusSequence serves /v1/status from a list, repeating the last, and
// answers /health 503 throughout, as a node does before a model has loaded.
func statusSequence(f *fake, seq ...meshapi.NodeStatus) {
	var n int
	var mu sync.Mutex
	f.node.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusServiceUnavailable)
		case meshapi.PathStatus:
			json.NewEncoder(w).Encode(seq[min(n, len(seq)-1)])
			n++
		case meshapi.PathCluster:
			st := seq[len(seq)-1]
			json.NewEncoder(w).Encode(meshapi.ClusterResponse{Members: []meshapi.Member{{Node: "node-a", State: meshapi.MemberAlive, Status: &st}}})
		}
	})
}

func backends(status string, names ...string) meshapi.NodeStatus {
	st := meshapi.NodeStatus{Node: "node-a", Addr: "127.0.0.1"}
	for _, n := range names {
		st.Models = append(st.Models, meshapi.ModelStatus{Name: n, Backends: []meshapi.BackendStatus{{ID: n + "-0", Status: status, Phase: "loading"}}})
	}
	return st
}

// A load that outlasts the wait for the process is not a failure: /health
// answers 503 until a model has loaded, and the model list starts empty.
func TestSlowLoadIsNotAFailure(t *testing.T) {
	f := newFake(t)
	f.host.UpTimeout = 300 * time.Millisecond
	statusSequence(f, meshapi.NodeStatus{Node: "node-a"}, meshapi.NodeStatus{Node: "node-a"},
		backends("unhealthy", "alpha", "beta"), backends("unhealthy", "alpha", "beta"), backends("unhealthy", "alpha", "beta"),
		backends(meshapi.StatusHealthy, "alpha", "beta"))
	if err := f.run(newMesh...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
}

func TestDeadBackendEndsTheWait(t *testing.T) {
	f := newFake(t)
	f.host.ReadyTimeout = 30 * time.Second
	st := backends(meshapi.StatusHealthy, "beta")
	st.Models = append(st.Models, backends(meshapi.StatusDead, "alpha").Models...)
	statusSequence(f, st)
	start := time.Now()
	if err := f.run(newMesh...); err == nil {
		t.Fatal("setup reported success")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("waited %v on a dead backend", d)
	}
	if !strings.Contains(f.out.String(), "alpha") || !strings.Contains(f.out.String(), "dead") {
		t.Errorf("output:\n%s", f.out)
	}
}

func TestNameTakenByANearbyNodeIsAskedAgain(t *testing.T) {
	f := newFake(t)
	f.host.Discover = func(context.Context) []discover.Node {
		return []discover.Node{{Name: "node-a\x1b[2J", Version: "v2.6.0", API: "127.0.0.1:8086"}, {Name: "node-a", API: "127.0.0.2:8086"}}
	}
	answers := slices.Clone(newMesh)
	answers[5] = "2"                              // a new secured mesh, though nodes were found
	answers = slices.Insert(answers, 8, "node-c") // the default node-a is taken
	if err := f.run(answers...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	cfg, _ := config.Parse(mustRead(t, f.path(install.ConfigFile)))
	if cfg.Node.Name != "node-c" || !strings.Contains(f.out.String(), "already") {
		t.Errorf("name %q\n%s", cfg.Node.Name, f.out)
	}
	if strings.Contains(f.out.String(), "\x1b") {
		t.Error("a discovered name reached the terminal with an escape sequence")
	}
}

// An edit naming a card this host lacks is refused, and the old cards kept:
// Validate cannot know the host, so the wizard checks against the probe.
func TestEditRefusesACardTheHostLacks(t *testing.T) {
	f := newFake(t)
	answers := slices.Clone(newMesh)
	// Edit alpha: GPUs "0 5", then the other fields unchanged; then accept.
	answers = slices.Insert(answers, 4, "2", "1", "0 5", "", "", "")
	if err := f.run(answers...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !strings.Contains(f.out.String(), "card 5 is not on this machine") {
		t.Errorf("output:\n%s", f.out)
	}
	cfg, _ := config.Parse(mustRead(t, f.path(install.ConfigFile)))
	if !slices.Equal(cfg.Models[0].GPUs, []int{0}) {
		t.Errorf("alpha gpus %v", cfg.Models[0].GPUs)
	}
}

// Compose expands $VAR inside a volume path, so /data/$x would mount /data/.
func TestModelsDirWithDollarIsAskedAgain(t *testing.T) {
	f := newFake(t)
	answers := append([]string{"/mo$dels"}, newMesh...)
	if err := f.run(answers...); err != nil {
		t.Fatalf("%v\n%s", err, f.out)
	}
	if !strings.Contains(f.out.String(), "'$'") {
		t.Errorf("output:\n%s", f.out)
	}
}

// In a container only the models directory is mounted: a model symlinked to
// a file outside it would be listed, planned, then missing. A link inside it
// is fine, and an unreadable directory is reported rather than skipped
// silently.
func TestScanAndSymlinks(t *testing.T) {
	root := t.TempDir()
	models := filepath.Join(root, "models")
	writeGGUF(t, filepath.Join(models, "alpha.gguf"), "llama", 1<<20)
	writeGGUF(t, filepath.Join(root, "elsewhere", "big.gguf"), "llama", 1<<20)
	os.Symlink(filepath.Join(root, "elsewhere", "big.gguf"), filepath.Join(models, "outside.gguf"))
	os.Symlink("alpha.gguf", filepath.Join(models, "inside.gguf"))
	os.MkdirAll(filepath.Join(models, "locked"), 0o755)
	os.Chmod(filepath.Join(models, "locked"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(models, "locked"), 0o755) })

	var out bytes.Buffer
	var files []string
	for _, w := range scan(prompt.Script(&out), models, true) {
		files = append(files, w.file)
	}
	if !slices.Equal(files, []string{"alpha.gguf", "inside.gguf"}) {
		t.Errorf("listed %q", files)
	}
	if !strings.Contains(out.String(), "skipped outside.gguf") || !strings.Contains(out.String(), "skipped locked") {
		t.Errorf("output:\n%s", out.String())
	}
	// Natively (a Mac), a link anywhere is readable, so it is listed.
	files = nil
	for _, w := range scan(prompt.Script(io.Discard), models, false) {
		files = append(files, w.file)
	}
	if !slices.Contains(files, "outside.gguf") {
		t.Errorf("native listed %q", files)
	}
}

// A pre-release is a release: it has published images. A private build of
// one (a -g<sha> or -dirty suffix) does not.
func TestPrereleaseIsARelease(t *testing.T) {
	for version, image := range map[string]bool{
		"v2.6.0-beta1":                true,
		"v2.6.0":                      true,
		"v2.6.0-beta1-gd266781":       false,
		"v2.5.0-gd266781":             false,
		"v2.6.0-beta1-gd266781-dirty": false,
		"dev":                         false,
	} {
		f := newFake(t)
		f.host.Version = version
		if err := f.run(newMesh...); err != nil {
			t.Fatalf("%s: %v\n%s", version, err, f.out)
		}
		pulled := slices.Contains(*f.calls, "docker pull ghcr.io/janit/viiwork-llamacpp-cuda:"+version)
		if pulled != image {
			t.Errorf("%s: pulled %v, want %v (calls %q)", version, pulled, image, *f.calls)
		}
	}
}
