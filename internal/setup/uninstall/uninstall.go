// Package uninstall is `viiwork uninstall`. It removes exactly what the
// install manifest lists, inside the locations an install uses, and nothing
// else. Model weights stay unless --delete-models.
package uninstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
	"github.com/janit/viiwork/v2/meshapi"
)

// Host is the machine the uninstall runs on. Tests replace every field.
type Host struct {
	GOOS    string
	Root    string // prefixed to every path; "" in production
	Home    string
	Euid    int
	UID     int // for launchctl's gui/<uid> domain
	Exec    install.Exec
	HTTP    *http.Client
	NodeAPI string
	Out     io.Writer
}

// Options are the command's flags.
type Options struct{ Yes, DeleteModels, KeepImages, FromConfig bool }

const (
	imagePrefix  = "ghcr.io/janit/viiwork-"
	volumePrefix = "viiwork_"
	launchAgent  = "fi.viiwork.node"
)

func (h Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h Host) configDir() string {
	if h.GOOS == "darwin" {
		return filepath.Join(h.Home, ".config", "viiwork")
	}
	return install.ConfigDir
}

func (h Host) manifestPath() string { return filepath.Join(h.configDir(), "install.json") }

// allowed reports whether an install could have created p.
func (h Host) allowed(p string) bool {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return false
	}
	if h.GOOS == "darwin" {
		return h.Home != "" && h.Home != "/" && strings.HasPrefix(p, h.Home+"/")
	}
	for _, dir := range []string{install.ConfigDir, install.StateDir, install.EngineDir} {
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return true
		}
	}
	for _, u := range install.EngineUnits {
		if p == filepath.Join(install.SystemdDir, u) {
			return true
		}
	}
	return p == install.BinaryPath || p == install.BinaryPath+".bak"
}

// throughLink reports whether a directory on p's way down is a symlink.
// os.Remove and RemoveAll follow such a link, so removing p would act on
// whatever it points at.
func (h Host) throughLink(p string) bool {
	for d := filepath.Dir(p); d != "/" && d != "."; d = filepath.Dir(d) {
		if fi, err := os.Lstat(h.path(d)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// removal is everything the uninstall will do, worked out before any of it.
type removal struct {
	m       install.Manifest
	files   []string
	dirs    []string
	binary  string
	images  []string
	volumes []string
	compose bool
	agent   bool
	helper  bool // the engine helper's units
	models  []string
	skipped []string
}

// Run is the uninstall.
func Run(ctx context.Context, h Host, p prompt.Prompter, o Options) error {
	if h.GOOS == "darwin" && h.Euid == 0 {
		// A Mac install lives in the user's home; as root, launchctl's gui
		// domain is the wrong one and the manifest is a file the user can
		// write, so root deletes would follow it anywhere.
		return errors.New("run without sudo: a Mac install lives in your home directory")
	}
	if h.GOOS == "linux" && h.Euid != 0 && !o.FromConfig {
		return errors.New("run as root (sudo viiwork uninstall): the install is in /etc/viiwork and /usr/local/bin")
	}
	m, err := install.ReadManifest(h.path(h.manifestPath()))
	if errors.Is(err, fs.ErrNotExist) {
		if o.FromConfig {
			return fromConfig(h, p)
		}
		return fmt.Errorf("no install manifest at %s: this machine was not set up by `viiwork init`, so uninstall will not guess what to remove. "+
			"`viiwork uninstall --from-config` lists what the config points at and removes nothing", h.manifestPath())
	}
	if err != nil {
		return err
	}
	if o.FromConfig {
		return fmt.Errorf("%s exists: run uninstall without --from-config", h.manifestPath())
	}

	r := h.plan(m, o)
	h.show(p, r)
	h.meshNote(ctx, p, m)
	if !o.Yes {
		word, err := p.Ask("Type uninstall to remove everything listed above", "")
		if err != nil {
			return err
		}
		if word != "uninstall" {
			return errors.New("not confirmed: nothing was removed")
		}
		if len(r.models) > 0 {
			word, err := p.Ask("Type delete models to delete the model files listed above", "")
			if err != nil {
				return err
			}
			if word != "delete models" {
				return errors.New("model deletion not confirmed: nothing was removed")
			}
		}
	}
	return h.execute(ctx, p, r)
}

func (h Host) plan(m install.Manifest, o Options) removal {
	r := removal{m: m}
	outside := "outside what an install creates"
	skip := func(what string) { r.skipped = append(r.skipped, what+": "+outside) }
	// safe is allowed, and not reached through a symlinked directory, where
	// removing it would act on whatever the link points at.
	safe := func(p string) bool {
		if !h.allowed(p) {
			skip(p)
			return false
		}
		if h.throughLink(p) {
			r.skipped = append(r.skipped, p+": a directory on its path is a symlink")
			return false
		}
		return true
	}
	for _, f := range m.Files {
		if safe(f) {
			r.files = append(r.files, f)
		}
	}
	for _, d := range m.Dirs {
		if safe(d) {
			r.dirs = append(r.dirs, d)
		}
	}
	sort.Slice(r.dirs, func(i, j int) bool { return strings.Count(r.dirs[i], "/") > strings.Count(r.dirs[j], "/") })
	if m.Binary != "" {
		if safe(m.Binary) {
			r.binary = m.Binary
		}
	}
	if c := m.Compose; c != nil {
		if c.Project == install.Project && h.allowed(c.File) {
			r.compose = true
			for _, v := range c.Volumes {
				if strings.HasPrefix(v, volumePrefix) {
					r.volumes = append(r.volumes, v)
				} else {
					skip(v)
				}
			}
		} else {
			skip("compose project " + c.Project + " (" + c.File + ")")
		}
	}
	if !o.KeepImages {
		for _, img := range m.Images {
			if strings.HasPrefix(img, imagePrefix) {
				r.images = append(r.images, img)
			} else {
				skip(img)
			}
		}
	}
	r.helper = h.GOOS == "linux" && m.EngineHelper != nil
	if r.helper {
		// The backups the helper keeps when it swaps the image and updates
		// the binary: its own files, though no manifest lists them.
		var backups []string
		if m.Compose != nil {
			backups = append(backups, m.Compose.File+".bak")
		}
		if m.Binary != "" {
			backups = append(backups, m.Binary+".bak")
		}
		for _, b := range backups {
			if _, err := os.Lstat(h.path(b)); err == nil && safe(b) {
				r.files = append(r.files, b)
			}
		}
	}
	r.agent = m.LaunchAgent == launchAgent
	if m.LaunchAgent != "" && !r.agent {
		skip("launch agent " + m.LaunchAgent)
	}
	if o.DeleteModels {
		r.models = h.modelFiles(r, func(what string) { r.skipped = append(r.skipped, what) })
	}
	return r
}

var shardRe = regexp.MustCompile(`^(.*)-00001-of-(\d{5})\.gguf$`)

// modelFiles are the weights the removed config served, inside the manifest's
// model directories: every shard of a split model, and only .gguf files.
func (h Host) modelFiles(r removal, skip func(string)) []string {
	var dirs []string
	for _, d := range r.m.ModelDirs {
		if !filepath.IsAbs(d) || filepath.Clean(d) != d || d == "/" {
			skip("models directory " + d + ": not a directory an install would record")
			continue
		}
		dirs = append(dirs, d)
	}
	var cfgPath string
	for _, f := range r.files {
		if filepath.Base(f) == "viiwork.yaml" {
			cfgPath = f
		}
	}
	data, err := os.ReadFile(h.path(cfgPath))
	if cfgPath == "" || err != nil {
		skip("model files: the config is not readable")
		return nil
	}
	cfg, err := config.Parse(data)
	if err != nil {
		skip("model files: " + err.Error())
		return nil
	}
	var out []string
	for _, m := range cfg.Models {
		p := m.Path
		if h.GOOS != "darwin" && len(dirs) > 0 && strings.HasPrefix(p, "/models/") {
			p = filepath.Join(dirs[0], strings.TrimPrefix(p, "/models/")) // the container's mount
		}
		p = filepath.Clean(p)
		inside := false
		for _, d := range dirs {
			inside = inside || strings.HasPrefix(p, d+"/")
		}
		if !inside || !strings.HasSuffix(p, ".gguf") {
			skip("model " + m.Path + ": outside the recorded models directories")
			continue
		}
		if fi, err := os.Lstat(h.path(p)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(h.path(p))
			skip("model " + m.Path + ": a symlink to " + target + "; delete the weights there yourself if you mean to")
			continue
		}
		if h.throughLink(p) {
			skip("model " + m.Path + ": a directory on its path is a symlink")
			continue
		}
		if s := shardRe.FindStringSubmatch(p); s != nil {
			n, _ := strconv.Atoi(s[2])
			for i := 1; i <= n; i++ {
				out = append(out, fmt.Sprintf("%s-%05d-of-%s.gguf", s[1], i, s[2]))
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

func (h Host) size(p string) int64 {
	var n int64
	filepath.WalkDir(h.path(p), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (h Host) show(p prompt.Prompter, r removal) {
	p.Say("viiwork uninstall removes:")
	if r.compose {
		p.Say("  the compose project %s (stopped first, so the node leaves the mesh)", install.Project)
	}
	if r.agent {
		p.Say("  the launch agent %s (stopped first, so the node leaves the mesh)", launchAgent)
	}
	if r.helper {
		p.Say("  the engine helper %s (stopped before the node)", strings.Join(install.EngineUnits, ", "))
	}
	for _, v := range r.volumes {
		p.Say("  volume %s", v)
	}
	for _, img := range r.images {
		p.Say("  image %s", img)
	}
	for _, f := range append(append(append([]string{}, r.files...), r.dirs...), r.binary) {
		if f != "" {
			p.Say("  %-60s %s", f, human(h.size(f)))
		}
	}
	for _, f := range r.models {
		p.Say("  model %-54s %s", f, human(h.size(f)))
	}
	if len(r.models) == 0 && len(r.m.ModelDirs) > 0 {
		p.Say("It keeps the model weights in %s (--delete-models removes the ones this node served).", strings.Join(r.m.ModelDirs, ", "))
	}
	for _, s := range r.skipped {
		p.Say("  skipped %s", s)
	}
}

// meshNote says what leaves the mesh with this node.
func (h Host) meshNote(ctx context.Context, p prompt.Prompter, m install.Manifest) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var c meshapi.ClusterResponse
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+h.NodeAPI+meshapi.PathCluster, nil)
	resp, err := h.HTTP.Do(req)
	if err == nil {
		defer resp.Body.Close()
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&c)
	}
	if err != nil {
		p.Say("The node is not answering, so uninstall cannot tell whether other members hold the alias table.")
		return
	}
	alive := 0
	for _, mem := range c.Members {
		if mem.State == meshapi.MemberAlive {
			alive++
		}
	}
	if alive > 1 {
		p.Say("%d other members hold the alias table, which is replicated: nothing is lost.", alive-1)
		return
	}
	secured := false
	for _, f := range m.Files {
		secured = secured || filepath.Base(f) == "mesh.env"
	}
	p.Say("This node is the mesh's last member: the alias table goes with it.")
	if secured {
		p.Say("So does the only copy of the mesh secret. Keep mesh.env or a join code if the mesh will be rebuilt.")
	}
}

func (h Host) execute(ctx context.Context, p prompt.Prompter, r removal) error {
	run := func(name string, args ...string) error { return h.Exec(ctx, h.Out, name, args...) }

	// Stop the node first. If that fails, touch nothing: a running node with
	// restart: always (or KeepAlive) would crash-loop on its deleted files,
	// and without the manifest nothing could undo it.
	if r.agent {
		target := "gui/" + strconv.Itoa(h.UID) + "/" + launchAgent
		if run("launchctl", "print", target) == nil { // loaded
			if err := run("launchctl", "bootout", target); err != nil {
				return fmt.Errorf("could not stop %s (%v): nothing was removed", launchAgent, err)
			}
		}
	}
	// The engine helper goes before the node: it runs `docker compose up`,
	// and left running it could start the node again mid-uninstall. Units
	// that were never written have nothing to stop.
	helperUnits := false
	for _, u := range install.EngineUnits {
		if _, err := os.Stat(h.path(filepath.Join(install.SystemdDir, u))); err == nil {
			helperUnits = true
		}
	}
	if r.helper && helperUnits {
		if err := run("systemctl", append([]string{"disable", "--now"}, install.EnginePath, install.EngineTimer, install.EngineService)...); err != nil {
			return fmt.Errorf("could not stop the engine helper (%v): nothing was removed", err)
		}
	}
	if r.compose {
		// The installer writes its manifest before the compose file; without
		// the file, nothing was ever started.
		if _, err := os.Stat(h.path(r.m.Compose.File)); err == nil {
			compose := []string{"compose", "-f", h.path(r.m.Compose.File), "-p", install.Project}
			if err := run("docker", append(compose, "stop", "-t", "90")...); err != nil {
				return fmt.Errorf("docker compose stop failed (%v): nothing was removed; is Docker running?", err)
			}
			if err := run("docker", append(compose, "down")...); err != nil {
				return fmt.Errorf("docker compose down failed (%v): nothing was removed", err)
			}
		}
		for _, v := range r.volumes {
			if err := run("docker", "volume", "rm", v); err != nil {
				p.Say("kept volume %s: %v", v, err)
			}
		}
	}
	for _, img := range r.images {
		if err := run("docker", "rmi", img); err != nil {
			p.Say("kept %s: %v (another container may use it, or it is already gone)", img, err)
		}
	}

	var failed []string
	remove := func(path string, all bool) {
		var err error
		if all {
			err = os.RemoveAll(h.path(path))
		} else {
			err = os.Remove(h.path(path))
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			p.Say("could not remove %s: %v", path, err)
			failed = append(failed, path)
		}
	}
	for _, f := range r.models {
		remove(f, false)
	}
	for _, f := range r.files {
		remove(f, false)
	}
	for _, d := range r.dirs {
		if d == h.configDir() {
			continue // removed below, and only when empty
		}
		remove(d, true)
	}
	if r.binary != "" {
		remove(r.binary, false) // a running binary may unlink its own file
	}
	if len(failed) > 0 {
		// The manifest stays, so that a rerun can finish the job.
		return fmt.Errorf("could not remove %s; the manifest is kept, so run uninstall again once that is fixed", strings.Join(failed, ", "))
	}
	if r.helper && helperUnits {
		if err := run("systemctl", "daemon-reload"); err != nil {
			p.Say("systemctl daemon-reload: %v", err)
		}
	}
	remove(h.manifestPath(), false)  // last: until here a rerun can finish
	os.Remove(h.path(h.configDir())) // only when empty: an operator's files stay
	p.Say("Removed.")
	return nil
}

// fromConfig lists what a hand-built host's config points at.
func fromConfig(h Host, p prompt.Prompter) error {
	cfgPath := filepath.Join(h.configDir(), "viiwork.yaml")
	data, err := os.ReadFile(h.path(cfgPath))
	if err != nil {
		return fmt.Errorf("no install manifest and no config at %s", cfgPath)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	p.Say("No install manifest: this host was set up by hand. From %s:", cfgPath)
	p.Say("  config     %s", cfgPath)
	p.Say("  state dir  %s", cfg.Node.StateDir)
	for _, m := range cfg.Models {
		p.Say("  model      %s (%s)", m.Path, m.Name)
	}
	p.Say("Nothing was removed. Remove a hand-built install by hand (docs/setup.md, docs/macos.md).")
	return nil
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
