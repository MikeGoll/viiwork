package nodectl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/setup/install"
)

type fake struct {
	h     Host
	out   *bytes.Buffer
	calls []string
	fail  map[string]bool // commands that fail, by their joined text
}

func (f *fake) write(p, data string) {
	os.MkdirAll(filepath.Dir(filepath.Join(f.h.Root, p)), 0o755)
	os.WriteFile(filepath.Join(f.h.Root, p), []byte(data), 0o644)
}

func (f *fake) manifest(p string, m install.Manifest) {
	m.Version = 1
	b, _ := json.Marshal(m)
	f.write(p, string(b))
}

// newFake is a host whose node API answers when up, and refuses when not.
func newFake(t *testing.T, goos string, up bool) *fake {
	f := &fake{out: &bytes.Buffer{}, fail: map[string]bool{}}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(node.Close)
	api := strings.TrimPrefix(node.URL, "http://")
	if !up {
		node.Close()
	}
	euid := 0
	if goos == "darwin" {
		euid = 501
	}
	f.h = Host{GOOS: goos, Root: t.TempDir(), Home: "/Users/me", Euid: euid, UID: 501,
		HTTP: node.Client(), NodeAPI: api, Out: f.out, Wait: 200 * time.Millisecond, Poll: 10 * time.Millisecond,
		Exec: func(_ context.Context, _ io.Writer, name string, args ...string) error {
			c := name + " " + strings.Join(args, " ")
			f.calls = append(f.calls, c)
			if f.fail[c] {
				return errors.New("exit 1")
			}
			return nil
		}}
	return f
}

func newMac(t *testing.T, up bool) *fake {
	f := newFake(t, "darwin", up)
	f.manifest(install.MacLayout(f.h.Home).ManifestFile, install.Manifest{OS: "darwin", LaunchAgent: install.LaunchAgent})
	return f
}

func newLinux(t *testing.T, up bool) *fake {
	f := newFake(t, "linux", up)
	f.manifest(install.ManifestFile, install.Manifest{OS: "linux",
		Compose:      &install.Compose{File: install.ComposeFile, Project: install.Project},
		EngineHelper: &install.EngineHelper{Units: install.EngineUnits, Dir: install.EngineDir}})
	f.write(install.ComposeFile, "name: viiwork\n")
	for _, u := range install.EngineUnits {
		f.write(filepath.Join(install.SystemdDir, u), "[Unit]\n")
	}
	return f
}

func (f *fake) compose(a string) string {
	return "docker compose -f " + filepath.Join(f.h.Root, install.ComposeFile) + " -p viiwork " + a
}

func TestStopMacBootsOutTheAgent(t *testing.T) {
	f := newMac(t, false)
	if err := Stop(context.Background(), f.h); err != nil {
		t.Fatal(err)
	}
	want := []string{"launchctl print gui/501/fi.viiwork.node", "launchctl bootout gui/501/fi.viiwork.node"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
	if !strings.Contains(f.out.String(), "next login") {
		t.Errorf("output %q does not say when it comes back", f.out)
	}
}

func TestStopMacNotLoadedDoesNothing(t *testing.T) {
	f := newMac(t, false)
	f.fail["launchctl print gui/501/fi.viiwork.node"] = true
	if err := Stop(context.Background(), f.h); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !strings.Contains(f.out.String(), "not running") {
		t.Fatalf("calls %q, output %q", f.calls, f.out)
	}
}

func TestStopMacRefusesRoot(t *testing.T) {
	f := newMac(t, false)
	f.h.Euid = 0
	if err := Stop(context.Background(), f.h); err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("ran %q", f.calls)
	}
}

func TestStopLinuxStopsTheHelperThenTheNode(t *testing.T) {
	f := newLinux(t, false)
	if err := Stop(context.Background(), f.h); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl stop viiwork-engine.path viiwork-engine.timer viiwork-engine.service",
		f.compose("stop -t 90"),
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
}

func TestStopLinuxLeavesTheNodeWhenTheHelperWillNotStop(t *testing.T) {
	f := newLinux(t, false)
	f.fail["systemctl stop viiwork-engine.path viiwork-engine.timer viiwork-engine.service"] = true
	if err := Stop(context.Background(), f.h); err == nil {
		t.Fatal("no error")
	}
	if len(f.calls) != 1 {
		t.Fatalf("went on after the helper failed: %q", f.calls)
	}
}

func TestStopLinuxWithoutTheHelper(t *testing.T) {
	f := newLinux(t, false)
	f.manifest(install.ManifestFile, install.Manifest{OS: "linux", Compose: &install.Compose{File: install.ComposeFile, Project: install.Project}})
	if err := Stop(context.Background(), f.h); err != nil {
		t.Fatal(err)
	}
	if want := []string{f.compose("stop -t 90")}; !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
}

func TestStopLinuxNeedsRoot(t *testing.T) {
	f := newLinux(t, false)
	f.h.Euid = 1000
	if err := Stop(context.Background(), f.h); err == nil || !strings.Contains(err.Error(), "sudo viiwork stop") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopWithoutManifestGuessesNothing(t *testing.T) {
	f := newFake(t, "linux", false)
	err := Stop(context.Background(), f.h)
	if err == nil || !strings.Contains(err.Error(), "not set up by `viiwork init`") {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("ran %q", f.calls)
	}
}

func TestStopFailsWhileTheAPIStillAnswers(t *testing.T) {
	f := newLinux(t, true)
	err := Stop(context.Background(), f.h)
	if err == nil || !strings.Contains(err.Error(), "still answers") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartMacBootstrapsTheAgent(t *testing.T) {
	f := newMac(t, true)
	f.fail["launchctl print gui/501/fi.viiwork.node"] = true
	if err := Start(context.Background(), f.h); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(f.h.Root, install.MacLayout(f.h.Home).Plist)
	if want := "launchctl bootstrap gui/501 " + plist; f.calls[len(f.calls)-1] != want {
		t.Fatalf("calls %q, want last %q", f.calls, want)
	}
}

func TestStartMacAlreadyRunning(t *testing.T) {
	f := newMac(t, true)
	if err := Start(context.Background(), f.h); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || !strings.Contains(f.out.String(), "already running") {
		t.Fatalf("calls %q, output %q", f.calls, f.out)
	}
}

func TestStartLinuxStartsTheNodeThenTheHelper(t *testing.T) {
	f := newLinux(t, true)
	if err := Start(context.Background(), f.h); err != nil {
		t.Fatal(err)
	}
	want := []string{f.compose("up -d"), "systemctl start viiwork-engine.path viiwork-engine.timer"}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q, want %q", f.calls, want)
	}
}

func TestStartFailsWhenTheAPINeverAnswers(t *testing.T) {
	f := newLinux(t, false)
	if err := Start(context.Background(), f.h); err == nil {
		t.Fatal("no error")
	}
}
