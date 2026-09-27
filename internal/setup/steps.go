package setup

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/janit/viiwork/v2/internal/accept"
	"github.com/janit/viiwork/v2/internal/config"
	"github.com/janit/viiwork/v2/internal/gguf"
	"github.com/janit/viiwork/v2/internal/gpu"
	"github.com/janit/viiwork/v2/internal/joincode"
	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/setup/install"
	"github.com/janit/viiwork/v2/internal/setup/plan"
	"github.com/janit/viiwork/v2/internal/setup/probe"
	"github.com/janit/viiwork/v2/internal/setup/prompt"
	"github.com/janit/viiwork/v2/internal/setup/render"
)

func preflight(ctx context.Context, h Host, configPath string) error {
	if configPath != install.ConfigFile {
		return fmt.Errorf("a Linux install writes %s, which its compose file mounts; run init without --config", install.ConfigFile)
	}
	if h.Euid != 0 {
		return errors.New("run as root (sudo viiwork init): the install writes /etc/viiwork and /usr/local/bin")
	}
	if _, err := os.Stat(h.path(install.ManifestFile)); err == nil {
		return fmt.Errorf("an earlier install is recorded in %s: remove it with `viiwork uninstall` first", install.ManifestFile)
	}
	// init never overwrites a file it would then claim in its manifest: an
	// existing mesh.env may be the only copy of a mesh's secret.
	for _, f := range []string{configPath, install.EnvFile, install.ComposeFile} {
		if _, err := os.Stat(h.path(f)); err == nil {
			return fmt.Errorf("%s already exists, and init never overwrites a file it did not create", f)
		}
	}
	if _, err := h.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return errors.New("Docker is not reachable: install Docker Engine (https://docs.docker.com/engine/install/) and start it")
	}
	if _, err := h.Run(ctx, "docker", "compose", "version"); err != nil {
		return errors.New("Docker Compose v2 is missing (`docker compose` is not a docker command): " +
			"install the compose plugin (https://docs.docker.com/compose/install/linux/), for example the docker-compose-plugin or docker-compose-v2 package")
	}
	if out, err := h.Run(ctx, "docker", "ps", "-a", "--filter", "name=^viiwork$", "--format", "{{.Names}}"); err == nil && strings.TrimSpace(string(out)) != "" {
		return errors.New("a container named viiwork already exists (a node set up by hand?); init leaves it alone, so remove it first if it is yours to remove")
	}
	for _, pt := range []struct {
		network string
		port    int
	}{{"tcp", 8086}, {"tcp", 7946}, {"udp", 7946}} {
		if !h.PortFree(pt.network, pt.port) {
			return fmt.Errorf("port %d/%s is in use, and a viiwork node needs it", pt.port, pt.network)
		}
	}
	return nil
}

// privateBuild is the suffix scripts/version.sh gives a build that is not
// exactly a release: -g<sha>, -dirty, or both.
var privateBuild = regexp.MustCompile(`-(g[0-9a-f]{7,40}(-dirty)?|dirty)$`)

// published reports whether version is a release, pre-releases included,
// and so has engine images on ghcr.
func published(version string) bool {
	return release.ValidVersion(version) && !privateBuild.MatchString(version)
}

func hardware(ctx context.Context, h Host, p prompt.Prompter, s *state) error {
	gpus, err := probe.GPUs(ctx, h.GOOS, h.Run, h.Sys)
	if err != nil || len(gpus) == 0 {
		return errors.New("no supported GPU found: NVIDIA cards are found through nvidia-smi, AMD cards through the kernel's KFD topology")
	}
	s.gpus, s.vendor = gpus, string(gpus[0].Vendor)
	if gpus[0].Vendor == gpu.VendorApple {
		// One GPU whose memory is the machine's: the budget is what Metal
		// lets it wire, and every model and every other program share it.
		s.budget = gpus[0].VRAMMB
		s.mac = install.MacLayout(h.Home)
		s.llama = install.LlamaServerPath(s.mac.LlamaRoot, h.LlamaPin)
		p.Say("GPU: %s, with a Metal budget of %s shared with everything else on this Mac.", clean(gpus[0].Name), mib(s.budget))
		p.Say("Engine: llama.cpp %s, fetched from its GitHub release.", h.LlamaPin)
		return nil
	}
	p.Say("GPUs on this machine:")
	for _, g := range gpus {
		p.Say("  %d  %-6s  %-28s  %-7s  %s", g.Index, g.Vendor, g.Name, g.Arch, mib(g.VRAMMB))
	}
	first := gpus[0]
	switch {
	case first.Vendor == gpu.VendorNVIDIA && (h.GOARCH == "amd64" || h.GOARCH == "arm64"):
		if !published(h.Version) {
			s.why = "this is a development build (" + h.Version + "), and only releases have published images"
			break
		}
		// Docker reaches the cards through the toolkit's CDI spec (preferred:
		// no runtime to register) or through its registered nvidia runtime.
		devices, _ := h.Run(ctx, "docker", "info", "--format", "{{json .DiscoveredDevices}}")
		runtimes, _ := h.Run(ctx, "docker", "info", "--format", "{{json .Runtimes}}")
		switch {
		case strings.Contains(string(devices), `"nvidia.com/gpu=all"`):
			s.cdi = true
		case strings.Contains(string(runtimes), `"nvidia"`):
		default:
			return errors.New("Docker cannot reach the NVIDIA cards: install the NVIDIA Container Toolkit " +
				"(https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/); with Docker 25 or newer its CDI spec " +
				"is enough (`nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`), otherwise run " +
				"`nvidia-ctk runtime configure --runtime=docker` and restart Docker")
		}
		s.image = imageRepo + ":" + h.Version
	case first.Vendor == gpu.VendorAMD && first.Arch == "gfx906":
		s.why = "gfx906 (Radeon VII, MI50) has no published image; build one from a checkout with `make docker`"
	case first.Vendor == gpu.VendorAMD:
		s.why = "there is no verified image for " + first.Arch + " yet"
	default:
		s.why = fmt.Sprintf("there is no published image for %s on %s", first.Vendor, h.GOARCH)
	}
	if s.image != "" {
		p.Say("Engine image: %s", s.image)
	} else {
		p.Say("No engine image: %s. Setup writes the config and starts nothing.", s.why)
	}
	return nil
}

var shardRe = regexp.MustCompile(`-(\d{5})-of-\d{5}\.gguf$`)

func chooseModels(_ context.Context, h Host, p prompt.Prompter, s *state) error {
	def := ""
	suggest := []string{"/models", "/srv/models", filepath.Join(h.Home, "models")}
	if s.vendor == string(gpu.VendorApple) {
		suggest = []string{filepath.Join(h.Home, "models"), "/models"}
	}
	for _, d := range suggest {
		if fi, err := os.Stat(h.path(d)); err == nil && fi.IsDir() {
			def = d
			break
		}
	}
	for {
		dir, err := p.Ask("Models directory", def)
		if err != nil {
			return err
		}
		// The Linux compose file mounts it, and Compose expands a '$'.
		if !filepath.IsAbs(dir) || strings.ContainsAny(dir, ":\"\\\n\r") || (s.vendor != string(gpu.VendorApple) && strings.Contains(dir, "$")) {
			p.Say("Give an absolute path without ':', '$', quotes or backslashes.")
			continue
		}
		dir = filepath.Clean(dir)
		found := scan(p, h.path(dir), s.vendor != string(gpu.VendorApple))
		if len(found) == 0 {
			p.Say("No GGUF models under %s.", dir)
			continue
		}
		opts := make([]string, len(found))
		for i, w := range found {
			opts[i] = fmt.Sprintf("%-48s %-10s %s", w.file, w.info.Arch, mib((w.info.Size+(1<<20)-1)>>20))
		}
		picked, err := p.Select("Models to serve", opts)
		if err != nil {
			return err
		}
		taken := map[string]bool{}
		for _, i := range picked {
			w := found[i]
			for {
				name, err := p.Ask("Name for "+w.file, defaultName(w.file))
				if err != nil {
					return err
				}
				if modelNameRe.MatchString(name) && !taken[name] {
					w.name, taken[name] = name, true
					break
				}
				p.Say("Use letters, digits, '.', '_' and '-', and a name not used above.")
			}
			s.models = append(s.models, w)
		}
		s.modelsDir = dir
		return nil
	}
}

var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// scan lists the models under root, three directories deep: first shards
// only, vision projectors (arch clip) left out, unreadable files and
// directories reported. In a container (container true) only root is
// mounted, so a model symlinked to a file outside it is reported and left
// out: it would be planned, then missing.
func scan(p prompt.Prompter, root string, container bool) []weights {
	var out []weights
	realRoot, _ := filepath.EvalSymlinks(root)
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, path)
		if err != nil {
			if rel != "." {
				p.Say("  skipped %s: %v", rel, err)
			}
			return nil
		}
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || strings.Count(rel, string(filepath.Separator)) >= 2) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), ".gguf") {
			return nil
		}
		if m := shardRe.FindStringSubmatch(d.Name()); m != nil && m[1] != "00001" {
			return nil
		}
		if container && d.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil || !strings.HasPrefix(target, realRoot+string(filepath.Separator)) {
				p.Say("  skipped %s: it links outside the models directory, which is all the container sees", rel)
				return nil
			}
		}
		info, err := gguf.Read(path)
		if err != nil {
			p.Say("  skipped %s: %v", rel, err)
			return nil
		}
		if info.Arch == "clip" {
			return nil
		}
		out = append(out, weights{file: rel, info: info})
		return nil
	})
	return out
}

func defaultName(file string) string {
	base := filepath.Base(file)
	if m := shardRe.FindStringIndex(base); m != nil {
		return base[:m[0]]
	}
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func layout(_ context.Context, _ Host, p prompt.Prompter, s *state) error {
	gpus := make([]plan.GPU, len(s.gpus))
	for i, g := range s.gpus {
		gpus[i] = plan.GPU{Index: g.Index, VRAMMB: g.VRAMMB}
	}
	models := make([]plan.Model, len(s.models))
	for i, m := range s.models {
		models[i] = plan.Model{Name: m.name, Info: m.info}
	}
	apple := s.vendor == string(gpu.VendorApple)
	var pl plan.Plan
	if apple {
		pl = plan.Unified(s.budget, models)
	} else {
		pl = plan.Discrete(gpus, models)
	}
	for _, r := range pl.Refused {
		p.Say("Not placed: %s — %s", r.Name, r.Reason)
	}
	placed := pl.Placed
	for {
		if len(placed) == 0 {
			return errors.New("no chosen model fits on these cards")
		}
		showPlan(p, placed)
		choice, err := p.Choose("Layout", []string{"Accept", "Edit a model", "Drop a model"}, 0)
		if err != nil {
			return err
		}
		names := make([]string, len(placed))
		for i, pl := range placed {
			names[i] = pl.Name
		}
		switch choice {
		case 0:
			check := *s
			check.placed, check.name, check.network, check.open = placed, "check", config.NetworkLAN, true
			if _, err := render.Config(check.answers(), nil); err != nil {
				p.Say("The node would refuse this layout: %v", err)
				continue
			}
			s.placed = placed
			return nil
		case 1:
			i, err := p.Choose("Which model", names, 0)
			if err != nil {
				return err
			}
			var cards []int // a Mac's one GPU is not asked about
			if !apple {
				for _, g := range s.gpus {
					cards = append(cards, g.Index)
				}
			}
			if err := edit(p, &placed[i], cards); err != nil {
				return err
			}
		case 2:
			i, err := p.Choose("Drop which model", names, 0)
			if err != nil {
				return err
			}
			placed = slices.Delete(placed, i, i+1)
		}
	}
}

// edit asks for one model's line. A malformed number keeps the old value.
// cards are the host's card numbers; nil on a Mac, whose one GPU every model
// shares, so the question is not asked.
func edit(p prompt.Prompter, pl *plan.Placement, cards []int) error {
	fields := []struct {
		q string
		v *int
	}{{"Context per slot (tokens)", &pl.Context}, {"Parallel slots", &pl.Parallel}}
	if cards != nil {
		answer, err := p.Ask("GPUs (card numbers)", joinInts(pl.GPUs))
		if err != nil {
			return err
		}
		var gpus []int
		for _, f := range strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(f)
			if err != nil {
				p.Say("%q is not a card number; GPUs unchanged.", f)
				gpus = pl.GPUs
				break
			}
			if !slices.Contains(cards, n) {
				p.Say("card %d is not on this machine (it has %s); GPUs unchanged.", n, joinInts(cards))
				gpus = pl.GPUs
				break
			}
			gpus = append(gpus, n)
		}
		pl.GPUs = gpus
		fields = append([]struct {
			q string
			v *int
		}{{"GPUs per backend", &pl.PerBackend}}, fields...)
	}
	for _, f := range fields {
		a, err := p.Ask(f.q, strconv.Itoa(*f.v))
		if err != nil {
			return err
		}
		if n, err := strconv.Atoi(a); err == nil && n > 0 {
			*f.v = n
		} else {
			p.Say("%q is not a positive number; %s unchanged.", a, f.q)
		}
	}
	pl.NeedMB = 0 // the estimate was for the planner's line
	return nil
}

func showPlan(p prompt.Prompter, placed []plan.Placement) {
	p.Say("\n  %-24s %-12s %-12s %-8s %-8s %s", "model", "cards", "per backend", "context", "parallel", "estimate")
	for _, pl := range placed {
		est := "edited"
		if pl.NeedMB > 0 {
			est = mib(pl.NeedMB) + " per backend"
			if pl.Uncertain {
				est += " (estimate uncertain)"
			}
		}
		p.Say("  %-24s %-12s %-12d %-8d %-8d %s", pl.Name, joinInts(pl.GPUs), pl.PerBackend, pl.Context, pl.Parallel, est)
	}
}

func meshStep(ctx context.Context, h Host, p prompt.Prompter, s *state) error {
	s.network = config.NetworkLAN
	if h.Tailnet(ctx) {
		s.network = config.NetworkTailnet
	}
	nodes := h.Discover(ctx)
	def := 1
	if len(nodes) > 0 {
		p.Say("viiwork nodes found nearby:")
		for _, n := range nodes {
			p.Say("  %-20s %-10s %-22s %s", clean(n.Name), clean(n.Version), clean(n.API), clean(strings.Join(n.Models, ", ")))
			s.nearby = append(s.nearby, n.Name)
		}
		def = 0
	} else {
		p.Say("No viiwork node answered nearby, which is normal for a first machine.")
	}
	for {
		choice, err := p.Choose("Mesh", []string{
			"Join an existing mesh (paste its join code)",
			"Start a new secured mesh",
			"Start an open mesh (no secret)",
		}, def)
		if err != nil {
			return err
		}
		switch choice {
		case 0:
			code, err := p.Ask("Join code (from `viiwork join-code` on a member)", "")
			if err != nil {
				return err
			}
			c, err := joincode.Decode(code)
			if err != nil {
				p.Say("%v", err)
				continue
			}
			s.secret, s.open, s.seeds = c.Secret, c.Secret == nil, []string{c.Seed}
			return nil
		case 1:
			s.secret = make([]byte, 32)
			if _, err := io.ReadFull(h.Rand, s.secret); err != nil {
				return err
			}
			s.open, s.seeds = false, nil
			return nil
		case 2:
			ok, err := p.Confirm("In an open mesh any host that reaches ports 8086 and 7946 can join and forward requests. Continue?", false)
			if err != nil {
				return err
			}
			if ok {
				s.secret, s.open, s.seeds = nil, true, nil
				return nil
			}
		}
	}
}

func updates(_ context.Context, _ Host, p prompt.Prompter, s *state) error {
	if s.open {
		p.Say("Mesh-wide updates need a secured mesh: in an open mesh, update.enabled stays off and updates are local only.")
		return nil
	}
	ok, err := p.Confirm("Accept mesh-wide updates (`viiwork update`) on this machine?", false)
	s.update = ok
	return err
}

var nodeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// NodeName makes a node name from a host name: the first label, lowercased,
// with anything outside [a-z0-9-] replaced by '-'.
func NodeName(hostname string) string {
	label, _, _ := strings.Cut(hostname, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	if n := strings.Trim(b.String(), "-"); n != "" {
		return n
	}
	return "node"
}

func naming(_ context.Context, h Host, p prompt.Prompter, s *state) error {
	host, _ := h.Hostname()
	def := NodeName(host)
	for {
		name, err := p.Ask("Node name", def)
		if err != nil {
			return err
		}
		if !nodeNameRe.MatchString(name) {
			p.Say("Use lowercase letters, digits and '-'.")
			continue
		}
		if slices.Contains(s.nearby, name) {
			// Two members with one name: the newer process exits, and
			// restart: always would loop it.
			p.Say("A node named %s is already in the mesh nearby; choose another name.", name)
			if name == def {
				def = ""
			}
			continue
		}
		s.name = name
		return nil
	}
}

func (s *state) answers() render.Answers {
	a := render.Answers{Node: s.name, Network: s.network, Open: s.open, Seeds: s.seeds, Update: s.update, Vendor: s.vendor}
	apple := s.vendor == string(gpu.VendorApple)
	if apple {
		a.StateDir = s.mac.StateDir // absolute: launchd expands nothing
	}
	for _, pl := range s.placed {
		for _, w := range s.models {
			if w.name != pl.Name {
				continue
			}
			m := render.Model{Name: pl.Name, Path: "/models/" + filepath.ToSlash(w.file),
				Size: w.info.Size, GPUs: pl.GPUs, PerBackend: pl.PerBackend, Context: pl.Context, Parallel: pl.Parallel}
			if apple {
				m.Path, m.Binary = filepath.Join(s.modelsDir, w.file), s.llama
			}
			a.Models = append(a.Models, m)
		}
	}
	return a
}

func (s *state) lookup(k string) (string, bool) {
	if k == config.DefaultMeshSecretEnv && s.secret != nil {
		return base64.StdEncoding.EncodeToString(s.secret), true
	}
	return "", false
}

func writeAndStart(ctx context.Context, h Host, p prompt.Prompter, s *state) error {
	cfg, err := render.Config(s.answers(), s.secret)
	if err != nil {
		return fmt.Errorf("the generated config was refused: %w", err)
	}
	files := []install.File{{Path: install.ConfigFile, Mode: 0o644, Data: cfg}}
	envFile := ""
	if s.secret != nil {
		envFile = install.EnvFile
		files = append(files, install.File{Path: install.EnvFile, Mode: 0o640, Data: render.MeshEnv(s.secret)})
	}
	m := install.Manifest{InstalledBy: h.Version, ModelDirs: []string{s.modelsDir}}
	if s.image != "" {
		compose, err := render.ComposeFile(render.Compose{Image: s.image, ModelsDir: s.modelsDir, ConfigFile: install.ConfigFile,
			EnvFile: envFile, StateDir: install.StateDir, Tailscale: s.network == config.NetworkTailnet, CDI: s.cdi})
		if err != nil {
			return err
		}
		files = append(files, install.File{Path: install.ComposeFile, Mode: 0o644, Data: compose})
		m.Compose = &install.Compose{File: install.ComposeFile, Project: install.Project}
		m.Images = []string{s.image}
	}

	p.Say("\nThese files will be written:")
	for _, f := range files {
		p.Say("\n── %s (mode %04o)", f.Path, f.Mode)
		if f.Path == install.EnvFile {
			p.Say("%s=******** (the mesh secret, 32 bytes)", config.DefaultMeshSecretEnv)
			continue
		}
		p.Say("%s", strings.TrimRight(string(f.Data), "\n"))
	}
	p.Say("\n── %s (a copy of this binary)", install.BinaryPath)
	p.Say("── %s (what uninstall will remove)", install.ManifestFile)
	q := "Write these files and start the node?"
	if s.image == "" {
		q = "Write these files?"
	}
	ok, err := p.Confirm(q, true)
	if err != nil {
		return err
	}
	if !ok {
		return prompt.ErrAbort
	}

	if h.Signals != nil {
		var stop context.CancelFunc
		ctx, stop = h.Signals(ctx)
		defer stop()
	}
	l := install.Linux{Root: h.Root, Exec: h.Exec, Out: h.Out, Executable: h.Executable, HTTP: h.HTTP, NodeAPI: h.NodeAPI}
	if _, err := l.Write(ctx, []string{install.ConfigDir, install.StateDir}, files, m); err != nil {
		return err
	}
	if s.image == "" {
		p.Say("\nThe config is written. Nothing was started: %s.", s.why)
		p.Say("Its model paths are /models/…: mount %s at /models in the container you run.", s.modelsDir)
		return nil
	}
	if err := l.Start(ctx, s.image); err != nil {
		return failed(ctx, p, l, err)
	}
	if err := l.WaitUp(ctx, h.UpTimeout); err != nil {
		return failed(ctx, p, l, err)
	}
	p.Say("The node is up. Waiting for every model to load (a large one takes minutes)…")
	var names []string
	for _, pl := range s.placed {
		names = append(names, pl.Name)
	}
	if err := l.WaitModels(ctx, names, h.ReadyTimeout, h.Poll); err != nil {
		return failed(ctx, p, l, err)
	}
	ready := accept.WaitReady(ctx, h.Accept, h.NodeAPI, time.Minute, h.Poll)
	if !ready.Pass() {
		var b bytes.Buffer
		ready.WriteText(&b)
		return failed(ctx, p, l, errors.New(strings.TrimSpace(b.String())))
	}
	return finish(ctx, h, p, s, ready, h.path(install.ConfigFile), h.path(s.modelsDir), []string{
		"Reload after editing " + install.ConfigFile + ": docker kill -s HUP viiwork",
		"Restart: docker " + strings.Join(l.ComposeArgs("restart"), " "),
		fmt.Sprintf("Files: %s, %s; node state in %s", install.ConfigDir, install.BinaryPath, install.StateDir),
	})
}

func failed(ctx context.Context, p prompt.Prompter, l install.Linux, err error) error {
	p.Say("\nThe node did not come up: %v", err)
	l.Diagnose(ctx)
	p.Say("The files are kept. Retry with `docker %s`, or undo everything with `viiwork uninstall`.",
		strings.Join(l.ComposeArgs("up", "-d"), " "))
	return err
}

// finish runs the acceptance checks, prints how to run the node (notes, per
// platform), and for a new secured mesh the join code for the next machine.
// cfgPath is the config as this process reads it; modelsRoot replaces a
// container's /models/ prefix ("" when the config's paths are the host's).
func finish(ctx context.Context, h Host, p prompt.Prompter, s *state, ready accept.Report, cfgPath, modelsRoot string, notes []string) error {
	_, cfgReport := accept.SummarizeConfig(cfgPath, s.lookup, modelsRoot, "")
	reports := []accept.Report{cfgReport, ready}
	if len(s.seeds) > 0 {
		ip, _, _ := net.SplitHostPort(s.seeds[0])
		var names []string
		for _, pl := range s.placed {
			names = append(names, pl.Name)
		}
		reports = append(reports, accept.WaitJoin(ctx, h.Accept, net.JoinHostPort(ip, strconv.Itoa(h.PeerAPIPort)),
			s.name, names, 2*time.Minute, 2*time.Second))
	}
	p.Say("")
	for _, r := range reports {
		r.WriteText(h.Out)
	}
	p.Say("\nDashboard: http://<this machine>:8086/ (the whole mesh: /mesh)")
	for _, n := range notes {
		p.Say("%s", n)
	}
	if s.secret != nil && len(s.seeds) == 0 {
		p.Say("\nThe next machine joins with this code:")
		joincode.Run(ctx, []string{"--node", h.NodeAPI, "--config", cfgPath}, joincode.Env{
			Stdout: h.Out, Stderr: h.Out, LookupEnv: s.lookup, Client: h.HTTP, ReadFile: os.ReadFile,
		})
	}
	for _, r := range reports {
		if !r.Pass() {
			return errors.New("setup finished, but a check above failed")
		}
	}
	return nil
}

// clean makes text another host sent safe to print: a control character
// would reach a root terminal as a live escape sequence.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, s)
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

func mib(v int64) string {
	if v >= 1024 {
		return fmt.Sprintf("%.1f GiB", float64(v)/1024)
	}
	return fmt.Sprintf("%d MiB", v)
}
