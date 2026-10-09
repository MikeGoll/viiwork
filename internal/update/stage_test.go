package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/release"
	"github.com/janit/viiwork/v2/internal/update/updatetest"
)

// fakeRelease serves a signed release of a shell-script "viiwork" for this
// host's target, at <source>/<version>/<asset>.
func fakeRelease(t *testing.T, version string, o updatetest.Opts) (string, ed25519.PublicKey) {
	t.Helper()
	assets, pub := updatetest.Assets(t, version, o)
	src, _ := updatetest.Serve(t, version, assets)
	return src, pub
}

func stager(dir, source string, pub ed25519.PublicKey) *Stager {
	return &Stager{Dir: dir, Source: source, Client: &http.Client{}, Keys: []ed25519.PublicKey{pub},
		Target: release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}}
}

func TestStage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, pub := fakeRelease(t, "v2.6.0", updatetest.Opts{Requires: `{"llamacpp":"b100"}`})
	st := stager(dir, src, pub)
	var asked map[string]string
	st.Engines = func(_ context.Context, req map[string]string) error { asked = req; return nil }
	if err := st.Stage(ctx, "v2.6.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := Binary(dir, "v2.6.0"); err != nil {
		t.Fatalf("staged binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v2.6.0", "viiwork-accept")); err != nil {
		t.Errorf("viiwork-accept not staged: %v", err)
	}
	if asked["llamacpp"] != "b100" {
		t.Errorf("engine requirements passed on: %v", asked)
	}
	// Staging the same signed bytes again touches nothing: a rollout retry
	// must never open a window with no release directory.
	before, _ := os.Stat(filepath.Join(dir, "v2.6.0"))
	if err := st.Stage(ctx, "v2.6.0"); err != nil {
		t.Fatalf("restage: %v", err)
	}
	after, _ := os.Stat(filepath.Join(dir, "v2.6.0"))
	if !os.SameFile(before, after) {
		t.Error("restaging identical bytes replaced the directory")
	}
	if got := Staged(dir); len(got) != 1 {
		t.Errorf("restage left %v", got)
	}
	// A damaged staged copy is replaced, and nothing is left beside it.
	os.WriteFile(filepath.Join(dir, "v2.6.0", "viiwork"), []byte("damaged"), 0o755)
	if err := st.Stage(ctx, "v2.6.0"); err != nil {
		t.Fatalf("restage over a damaged copy: %v", err)
	}
	if _, err := Binary(dir, "v2.6.0"); err != nil {
		t.Errorf("after restage: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestStageRefuses(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		opts updatetest.Opts
		want error
		text string
	}{
		"wrong key":        {updatetest.Opts{OtherKey: true}, ErrUnverified, "signature"},
		"unsigned":         {updatetest.Opts{Unsigned: true}, nil, "SHA256SUMS.sig"},
		"tampered archive": {updatetest.Opts{Tamper: true}, ErrUnverified, "sha256"},
		"wrong --version":  {updatetest.Opts{Reports: "v9.9.9"}, ErrUnverified, "v9.9.9"},
		"cannot run":       {updatetest.Opts{Shebang: "#!/nonexistent/interpreter"}, nil, "cannot run"},
	}
	for name, c := range cases {
		dir := t.TempDir()
		src, pub := fakeRelease(t, "v2.6.0", c.opts)
		err := stager(dir, src, pub).Stage(ctx, "v2.6.0")
		if err == nil || c.want != nil && !errors.Is(err, c.want) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", name, err)
		}
		if got := Staged(dir); len(got) != 0 {
			t.Errorf("%s: left %v staged", name, got)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%s: left %v behind", name, entries)
		}
	}

	dir := t.TempDir()
	src, pub := fakeRelease(t, "v2.6.0", updatetest.Opts{Requires: `{"llamacpp":"b999999"}`})
	st := stager(dir, src, pub)
	st.Engines = func(context.Context, map[string]string) error {
		return fmt.Errorf("%w: needs b999999", ErrEngineTooOld)
	}
	if err := st.Stage(ctx, "v2.6.0"); !errors.Is(err, ErrEngineTooOld) || len(Staged(dir)) != 0 {
		t.Errorf("engine too old: %v, staged %v", err, Staged(dir))
	}

	if err := stager(t.TempDir(), src, pub).Stage(ctx, "../../etc"); !errors.Is(err, ErrUnverified) {
		t.Errorf("a path as version: %v", err)
	}
	other := stager(t.TempDir(), src, pub)
	other.Target = release.Target{OS: "plan9", Arch: "mips"}
	if err := other.Stage(ctx, "v2.6.0"); err == nil {
		t.Error("staged for a target no release is built for")
	}
	if err := stager(t.TempDir(), src, pub).Stage(ctx, "v2.7.0"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing release: %v", err)
	}
}

// PrepareLlama sees the staged binary's pin after it proved it runs and
// before the engine check, and the check it returns judges the requirement
// against the build the release will run, in place of Engines.
func TestStagePreparesTheReleasesLlama(t *testing.T) {
	src, pub := fakeRelease(t, "v2.6.1", updatetest.Opts{Requires: `{"llamacpp":"b200"}`,
		Info: `{"version":"v2.6.1","llama_cpp":"b200","llama_cpp_macos_sha256":"abc"}`})
	st := stager(t.TempDir(), src, pub)
	var order []string
	var got BuildInfo
	st.Engines = func(context.Context, map[string]string) error { order = append(order, "engines"); return nil }
	st.PrepareLlama = func(_ context.Context, info BuildInfo) (func(context.Context, map[string]string) error, error) {
		order, got = append(order, "prepare"), info
		return func(_ context.Context, req map[string]string) error {
			order = append(order, "check "+req["llamacpp"])
			return nil
		}, nil
	}
	if err := st.Stage(context.Background(), "v2.6.1"); err != nil {
		t.Fatal(err)
	}
	if got != (BuildInfo{Version: "v2.6.1", LlamaCpp: "b200", LlamaCppMacSHA256: "abc"}) {
		t.Errorf("build info %+v", got)
	}
	if strings.Join(order, ",") != "prepare,check b200" {
		t.Errorf("order %q", order)
	}
}

func TestStageFailsWhenTheLlamaCannotBePrepared(t *testing.T) {
	src, pub := fakeRelease(t, "v2.6.1", updatetest.Opts{Info: `{"version":"v2.6.1","llama_cpp":"b200"}`})
	dir := t.TempDir()
	st := stager(dir, src, pub)
	st.PrepareLlama = func(context.Context, BuildInfo) (func(context.Context, map[string]string) error, error) {
		return nil, errors.New("download failed")
	}
	if err := st.Stage(context.Background(), "v2.6.1"); err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Fatalf("err %v", err)
	}
	if staged := Staged(dir); len(staged) != 0 {
		t.Errorf("staged %q", staged)
	}
}

// A release built before --build-info carries no pin: nothing to prepare,
// and Engines checks the engine as configured, which is what it will run.
func TestStageWithoutBuildInfoPassesNoPin(t *testing.T) {
	src, pub := fakeRelease(t, "v2.6.1", updatetest.Opts{})
	st := stager(t.TempDir(), src, pub)
	engines := false
	st.Engines = func(context.Context, map[string]string) error { engines = true; return nil }
	var got *BuildInfo
	st.PrepareLlama = func(_ context.Context, info BuildInfo) (func(context.Context, map[string]string) error, error) {
		got = &info
		return nil, nil
	}
	if err := st.Stage(context.Background(), "v2.6.1"); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.LlamaCpp != "" || !engines {
		t.Errorf("info %+v, engines checked %v", got, engines)
	}
}

// Releases come from GitHub: a redirect may stay on the source's host, but
// one to any other host is refused before that host is asked, signed
// release or not.
func TestStageFollowsRedirectsOnlyToGitHub(t *testing.T) {
	ctx := context.Background()
	real, pub := fakeRelease(t, "v2.6.0", updatetest.Opts{})
	var asked bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = true
		http.Redirect(w, r, real+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(elsewhere.Close)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/moved/") {
			http.Redirect(w, r, elsewhere.URL+strings.TrimPrefix(r.URL.Path, "/moved"), http.StatusFound)
			return
		}
		http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusFound) // same host: followed
	}))
	t.Cleanup(front.Close)
	dir := t.TempDir()
	err := stager(dir, front.URL, pub).Stage(ctx, "v2.6.0")
	if !errors.Is(err, ErrUnverified) || !strings.Contains(err.Error(), "not GitHub") {
		t.Fatalf("stage through a foreign redirect: %v", err)
	}
	if asked {
		t.Error("the foreign host was asked")
	}
	if got := Staged(dir); len(got) != 0 {
		t.Errorf("left behind: %v", got)
	}
}

var thisTarget = release.Target{OS: runtime.GOOS, Arch: runtime.GOARCH}

// answering is a Source that answers every request with code.
func answering(t *testing.T, code int) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

// gone is a Source nobody listens on.
func gone(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

// slow is a Source that answers after d.
func slow(t *testing.T, d time.Duration) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func localDir(dir string) func(context.Context, string) (string, error) {
	return func(context.Context, string) (string, error) { return dir, nil }
}

func collect(st *Stager) *[]string {
	var logs []string
	st.Log = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	return &logs
}

// GitHub up: the signed files come from GitHub, the archive from parrot.
func TestStageArchiveFromLocal(t *testing.T) {
	assets, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	src, hits := updatetest.Serve(t, "v2.6.0", assets)
	st := stager(t.TempDir(), src, pub)
	st.Local = localDir(updatetest.WriteDir(t, assets))
	if err := st.Stage(context.Background(), "v2.6.0"); err != nil {
		t.Fatal(err)
	}
	name := release.ArchiveName("v2.6.0", thisTarget)
	if hits.Get("SHA256SUMS") != 1 || hits.Get("SHA256SUMS.sig") != 1 || hits.Get(name) != 0 {
		t.Errorf("GitHub requests: SHA256SUMS %d, sig %d, archive %d; want 1, 1, 0",
			hits.Get("SHA256SUMS"), hits.Get("SHA256SUMS.sig"), hits.Get(name))
	}
}

// GitHub up: parrot's own signed files are never read, so parrot cannot
// choose the release — its archive must match GitHub's sums.
func TestStageIgnoresLocalSignedFilesWhileGitHubAnswers(t *testing.T) {
	github, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	src, _ := updatetest.Serve(t, "v2.6.0", github)
	other, _ := updatetest.Assets(t, "v2.6.0", updatetest.Opts{Reports: "v2.6.0", Requires: `{"x":"1"}`}) // different bytes, signed by another key
	st := stager(t.TempDir(), src, pub)
	st.Local = localDir(updatetest.WriteDir(t, other))
	logs := collect(st)
	if err := st.Stage(context.Background(), "v2.6.0"); err != nil {
		t.Fatal(err)
	}
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "sha256") {
		t.Errorf("logs %q: parrot's archive should have failed GitHub's sums", *logs)
	}
}

// GitHub up, parrot's archive unusable: the archive comes from GitHub, and
// the reason is logged once.
func TestStageArchiveFallsBackToGitHub(t *testing.T) {
	good, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	older, _ := updatetest.Assets(t, "v2.5.0", updatetest.Opts{})
	tampered, _ := updatetest.Assets(t, "v2.6.0", updatetest.Opts{Tamper: true})
	name := release.ArchiveName("v2.6.0", thisTarget)
	withLink := updatetest.WriteDir(t, good)
	os.Remove(filepath.Join(withLink, name))
	os.Symlink(filepath.Join(updatetest.WriteDir(t, good), name), filepath.Join(withLink, name))

	cases := map[string]struct {
		local func(context.Context, string) (string, error)
		why   string
	}{
		"local error":           {func(context.Context, string) (string, error) { return "", errors.New("parrot is away") }, "parrot is away"},
		"missing directory":     {localDir("/nonexistent/viiwork-parrot/v2.6.0"), "/nonexistent"},
		"other version's files": {localDir(updatetest.WriteDir(t, older)), name},
		"tampered archive":      {localDir(updatetest.WriteDir(t, tampered)), "sha256"},
		"symlinked archive":     {localDir(withLink), "not a regular file"},
	}
	for label, c := range cases {
		src, hits := updatetest.Serve(t, "v2.6.0", good)
		st := stager(t.TempDir(), src, pub)
		st.Local = c.local
		logs := collect(st)
		if err := st.Stage(context.Background(), "v2.6.0"); err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		if hits.Get(name) != 1 {
			t.Errorf("%s: archive fetched from GitHub %d times, want 1", label, hits.Get(name))
		}
		if len(*logs) != 1 || !strings.Contains((*logs)[0], c.why) || !strings.Contains((*logs)[0], "GitHub") {
			t.Errorf("%s: logs %q, want one line naming %q", label, *logs, c.why)
		}
	}
}

// GitHub unreachable: parrot's signed files are used, verified against the
// compiled-in key, and the stage says so.
func TestStageSignedFilesFromLocalWhileGitHubUnreachable(t *testing.T) {
	good, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	for label, src := range map[string]string{
		"no connection": gone(t),
		"503":           answering(t, 503),
		"429":           answering(t, 429),
		"timeout":       slow(t, 5*time.Second),
	} {
		st := stager(t.TempDir(), src, pub)
		st.MetaTimeout = 100 * time.Millisecond
		st.Local = localDir(updatetest.WriteDir(t, good))
		logs := collect(st)
		if err := st.Stage(context.Background(), "v2.6.0"); err != nil {
			t.Errorf("%s: %v", label, err)
			continue
		}
		if len(*logs) != 1 || !strings.Contains((*logs)[0], "GitHub unreachable") {
			t.Errorf("%s: logs %q", label, *logs)
		}
	}
}

// GitHub unreachable and parrot wrong or empty: the stage fails; there is
// nowhere else to go and nothing unverified is staged.
func TestStageFailsWhenGitHubUnreachableAndLocalUnusable(t *testing.T) {
	_, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	wrongKey, _ := updatetest.Assets(t, "v2.6.0", updatetest.Opts{OtherKey: true})
	tampered, _ := updatetest.Assets(t, "v2.6.0", updatetest.Opts{Tamper: true})
	for label, c := range map[string]struct {
		local func(context.Context, string) (string, error)
		want  error
	}{
		"wrong key":        {localDir(updatetest.WriteDir(t, wrongKey)), ErrUnverified},
		"tampered archive": {localDir(updatetest.WriteDir(t, tampered)), ErrUnverified},
		"parrot away":      {func(context.Context, string) (string, error) { return "", errors.New("away") }, nil},
	} {
		dir := t.TempDir()
		st := stager(dir, gone(t), pub)
		st.Local = c.local
		err := st.Stage(context.Background(), "v2.6.0")
		if err == nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v", label, err)
		}
		if got := Staged(dir); len(got) != 0 {
			t.Errorf("%s: staged %v", label, got)
		}
	}
}

// A definite answer from GitHub is final: parrot is not asked, so a release
// pulled from GitHub cannot come back through the swarm.
func TestStageGitHubsAnswerIsFinal(t *testing.T) {
	good, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	wrongKey, _ := updatetest.Assets(t, "v2.6.0", updatetest.Opts{OtherKey: true})
	wrongSrc, _ := updatetest.Serve(t, "v2.6.0", wrongKey)
	for label, c := range map[string]struct {
		src  string
		text string
	}{
		"404":           {answering(t, 404), "404"},
		"403":           {answering(t, 403), "403"},
		"bad signature": {wrongSrc, "signature"},
	} {
		asked := false
		st := stager(t.TempDir(), c.src, pub)
		st.Local = func(context.Context, string) (string, error) { asked = true; return updatetest.WriteDir(t, good), nil }
		err := st.Stage(context.Background(), "v2.6.0")
		if err == nil || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", label, err)
		}
		if asked {
			t.Errorf("%s: parrot was asked after GitHub's definite answer", label)
		}
	}
}

// A stage cancelled while Local waits does not go on to download.
func TestStageCancelledWhileLocal(t *testing.T) {
	good, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	src, hits := updatetest.Serve(t, "v2.6.0", good)
	st := stager(t.TempDir(), src, pub)
	ctx, cancel := context.WithCancel(context.Background())
	st.Local = func(ctx context.Context, _ string) (string, error) { cancel(); return "", ctx.Err() }
	if err := st.Stage(ctx, "v2.6.0"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if n := hits.Get(release.ArchiveName("v2.6.0", thisTarget)); n != 0 {
		t.Errorf("archive requested %d times after cancellation", n)
	}
}

// GitHub answered, just not with something usable: a redirect off GitHub,
// or signed files over their size cap. That is an answer, so it is final
// and parrot is not asked.
func TestStageAnsweredButUnusableIsFinal(t *testing.T) {
	good, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	elsewhere := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(elsewhere.Close)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	oversize := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("x"), 65<<10))
	}))
	t.Cleanup(oversize.Close)
	for label, c := range map[string]struct{ src, text string }{
		"redirect off GitHub": {redirecting.URL, "not GitHub"},
		"oversize SHA256SUMS": {oversize.URL, "larger than"},
	} {
		asked := false
		st := stager(t.TempDir(), c.src, pub)
		st.Local = func(context.Context, string) (string, error) { asked = true; return updatetest.WriteDir(t, good), nil }
		err := st.Stage(context.Background(), "v2.6.0")
		if err == nil || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v", label, err)
		}
		if asked {
			t.Errorf("%s: parrot was asked after GitHub answered", label)
		}
	}
}

// A FIFO in parrot's directory is refused without blocking the stage: the
// file is opened non-blocking and judged by what was opened, not by a stat
// taken before it.
func TestStageRefusesAFIFOFromLocal(t *testing.T) {
	good, pub := updatetest.Assets(t, "v2.6.0", updatetest.Opts{})
	src, hits := updatetest.Serve(t, "v2.6.0", good)
	dir := updatetest.WriteDir(t, good)
	name := release.ArchiveName("v2.6.0", thisTarget)
	os.Remove(filepath.Join(dir, name))
	if err := syscall.Mkfifo(filepath.Join(dir, name), 0o644); err != nil {
		t.Skip("no FIFOs here:", err)
	}
	st := stager(t.TempDir(), src, pub)
	st.Local = localDir(dir)
	logs := collect(st)
	done := make(chan error, 1)
	go func() { done <- st.Stage(context.Background(), "v2.6.0") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stage hung on a FIFO")
	}
	if hits.Get(name) != 1 || len(*logs) != 1 {
		t.Errorf("archive from GitHub %d times, logs %q", hits.Get(name), *logs)
	}
}
