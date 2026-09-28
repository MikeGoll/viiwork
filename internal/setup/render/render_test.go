package render

import (
	"encoding/base64"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/accept"
	"github.com/janit/viiwork/v2/internal/config"
	"gopkg.in/yaml.v3"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func answers() Answers {
	return Answers{
		Node: "node-a", Network: config.NetworkLAN, Vendor: config.VendorNVIDIA, Update: true,
		Models: []Model{
			{Name: "alpha", Path: "/models/alpha.gguf", Size: 7 << 30, GPUs: []int{0, 1}, PerBackend: 1, Context: 32768, Parallel: 2},
			{Name: "beta", Path: "/models/beta.gguf", Size: 30 << 30, GPUs: []int{2, 3}, PerBackend: 2, Context: 16384, Parallel: 2},
		},
	}
}

func lookup(secret []byte) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if k == config.DefaultMeshSecretEnv && secret != nil {
			return base64.StdEncoding.EncodeToString(secret), true
		}
		return "", false
	}
}

func TestConfigSecured(t *testing.T) {
	data, err := Config(answers(), secret)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(lookup(secret)); err != nil {
		t.Fatal(err)
	}
	if cfg.Node.Name != "node-a" || cfg.Mesh.Open || !cfg.Update.Enabled || len(cfg.Models) != 2 {
		t.Fatalf("config %+v", cfg)
	}
	b := cfg.Models[1]
	if b.GPUsPerBackend != 2 || b.Context != 16384 || b.Engine != "llamacpp" {
		t.Errorf("beta %+v", b)
	}
	// A split backend and weights over 20 GiB load slowly; the small one
	// keeps the engine default.
	if b.StartupTimeout.Minutes() != 45 || cfg.Models[0].StartupTimeout.Duration != 0 {
		t.Errorf("startup timeouts %v, %v", cfg.Models[0].StartupTimeout, b.StartupTimeout)
	}
	if strings.Contains(string(data), base64.StdEncoding.EncodeToString(secret)) {
		t.Error("the secret is in viiwork.yaml")
	}
}

func TestConfigOpenWithSeeds(t *testing.T) {
	a := answers()
	a.Open, a.Update, a.Seeds = true, false, []string{"192.0.2.10:7946"}
	data, err := Config(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Parse(data)
	if !cfg.Mesh.Open || len(cfg.Mesh.Seeds) != 1 || cfg.Update.Enabled {
		t.Errorf("mesh %+v update %+v", cfg.Mesh, cfg.Update)
	}
}

func TestConfigRefusals(t *testing.T) {
	if _, err := Config(answers(), nil); err == nil {
		t.Error("neither secured nor open was accepted")
	}
	a := answers()
	a.Models[1].GPUs = []int{1, 2} // card 1 is alpha's
	if _, err := Config(a, secret); err == nil || !strings.Contains(err.Error(), "gpu") {
		t.Errorf("a shared card: %v", err)
	}
}

// The file must pass the same config check the wizard runs at the end.
func TestConfigPassesAcceptCheck(t *testing.T) {
	dir := t.TempDir()
	models := filepath.Join(dir, "models")
	os.MkdirAll(models, 0o755)
	os.WriteFile(filepath.Join(models, "alpha.gguf"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(models, "beta.gguf"), []byte("x"), 0o644)
	data, err := Config(answers(), secret)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "viiwork.yaml")
	os.WriteFile(path, data, 0o644)
	_, r := accept.SummarizeConfig(path, lookup(secret), models, "secured")
	if !r.Pass() {
		t.Errorf("accept config check: %+v", r.Checks)
	}
}

func TestMeshEnv(t *testing.T) {
	want := "VIIWORK_MESH_SECRET=" + base64.StdEncoding.EncodeToString(secret) + "\n"
	if got := string(MeshEnv(secret)); got != want {
		t.Errorf("%q", got)
	}
}

func compose() Compose {
	return Compose{Image: "ghcr.io/janit/viiwork-llamacpp-cuda:v2.6.0", ModelsDir: "/srv/my models",
		ConfigDir: "/etc/viiwork", EnvFile: "/etc/viiwork/mesh.env", StateDir: "/var/lib/viiwork", Tailscale: true}
}

func TestComposeFile(t *testing.T) {
	data, err := ComposeFile(compose())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Name     string `yaml:"name"`
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Restart     string            `yaml:"restart"`
			Network     string            `yaml:"network_mode"`
			PID         string            `yaml:"pid"`
			Volumes     []string          `yaml:"volumes"`
			EnvFile     []string          `yaml:"env_file"`
			StopGrace   string            `yaml:"stop_grace_period"`
			Environment map[string]string `yaml:"environment"`
			Deploy      struct {
				Resources struct {
					Reservations struct {
						Devices []struct {
							Driver string `yaml:"driver"`
						} `yaml:"devices"`
					} `yaml:"reservations"`
				} `yaml:"resources"`
			} `yaml:"deploy"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	s := doc.Services["viiwork"]
	if doc.Name != "viiwork" || s.Image != compose().Image || s.Restart != "always" || s.Network != "host" || s.PID != "host" || s.StopGrace != "90s" {
		t.Errorf("service %+v", s)
	}
	want := []string{"/srv/my models:/models:ro", "/etc/viiwork:/etc/viiwork:ro", "/var/lib/viiwork:/var/lib/viiwork", "/var/run/tailscale:/var/run/tailscale:ro"}
	if strings.Join(s.Volumes, "|") != strings.Join(want, "|") {
		t.Errorf("volumes %q", s.Volumes)
	}
	if len(s.EnvFile) != 1 || s.EnvFile[0] != "/etc/viiwork/mesh.env" {
		t.Errorf("env_file %q", s.EnvFile)
	}
	if len(s.Deploy.Resources.Reservations.Devices) != 1 || s.Deploy.Resources.Reservations.Devices[0].Driver != "nvidia" {
		t.Errorf("devices %+v", s.Deploy)
	}

	c := compose()
	c.EnvFile, c.Tailscale = "", false
	data, _ = ComposeFile(c)
	if strings.Contains(string(data), "env_file") || strings.Contains(string(data), "tailscale") {
		t.Errorf("open mesh without tailscaled:\n%s", data)
	}
}

func TestComposeRefusesUnsafePaths(t *testing.T) {
	for _, dir := range []string{"models", "/a:b", `/a"b`, "/a\nb", "/data/$x"} { // Compose expands $x
		c := compose()
		c.ModelsDir = dir
		if _, err := ComposeFile(c); err == nil {
			t.Errorf("%q accepted", dir)
		}
	}
}

func TestPlist(t *testing.T) {
	in := PlistInput{Binary: "/Users/u/.local/bin/viiwork", Config: "/Users/u/.config/viiwork/viiwork.yaml",
		Log: "/Users/u/Library/Logs/a&b<c/viiwork.log", Secret: secret}
	data := Plist(in)
	if err := xml.Unmarshal(data, new(struct{ XMLName xml.Name })); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	s := string(data)
	for _, want := range []string{
		"<string>fi.viiwork.node</string>", "<string>/Users/u/.local/bin/viiwork</string>", "<string>--config</string>",
		"<string>/Users/u/.config/viiwork/viiwork.yaml</string>", "a&amp;b&lt;c", "<integer>90</integer>",
		"<key>KeepAlive</key>", "/usr/bin:/bin:/usr/sbin:/sbin", "<key>VIIWORK_MESH_SECRET</key>",
		base64.StdEncoding.EncodeToString(secret),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	in.Secret = nil
	if strings.Contains(string(Plist(in)), "VIIWORK_MESH_SECRET") {
		t.Error("an open mesh's plist names a secret")
	}
}

// With the NVIDIA Container Toolkit's CDI spec, the cards are named devices
// and no runtime needs registering with Docker (found on an Ubuntu 26.04 host
// where `driver: nvidia` failed and `nvidia.com/gpu=all` worked).
func TestComposeFileCDI(t *testing.T) {
	c := compose()
	c.CDI = true
	data, err := ComposeFile(c)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Devices []string       `yaml:"devices"`
			Deploy  map[string]any `yaml:"deploy"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	s := doc.Services["viiwork"]
	if len(s.Devices) != 1 || s.Devices[0] != "nvidia.com/gpu=all" || s.Deploy != nil {
		t.Errorf("devices %q deploy %v\n%s", s.Devices, s.Deploy, data)
	}
}

// The config is mounted by its directory, never as a single file: a file
// bind mount pins the inode, so an edit saved as a new file (sed -i, many
// editors) stays invisible to the node, and a SIGHUP reloads the old config
// (found on teddy, the first real install).
func TestComposeMountsTheConfigDirectory(t *testing.T) {
	data, err := ComposeFile(compose())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "viiwork.yaml:/etc") {
		t.Errorf("a single-file config mount:\n%s", data)
	}
	if !strings.Contains(string(data), `"/etc/viiwork:/etc/viiwork:ro"`) {
		t.Errorf("no directory mount:\n%s", data)
	}
}
