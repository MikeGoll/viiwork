package accept

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sourcedConfig is writeEngineConfig with the model sourced from
// viiwork-parrot at api.
func sourcedConfig(t *testing.T, id, api string) string {
	t.Helper()
	path := writeEngineConfig(t, "llamacpp", "    gpus: [0]\n    parallel: 2\n    context: 4096\n")
	replaceInFile(t, path, "path: /models/m", "source: viiwork-parrot:"+id)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fmt.Fprintf(f, "viiwork_parrot:\n  api: %s\n", api)
	return path
}

func fakeParrot(t *testing.T, statusJSON string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/status" {
			t.Errorf("accept must only read: got %s %s", r.Method, r.URL.Path)
			http.Error(w, "no", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, statusJSON)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestSourcedWeightsSeeding(t *testing.T) {
	weights := filepath.Join(t.TempDir(), "g.gguf")
	if err := os.WriteFile(weights, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	api := fakeParrot(t, fmt.Sprintf(`[{"id":"g","state":"seeding","percent":100,"path":%q}]`, weights))
	sum, r := SummarizeConfig(sourcedConfig(t, "g", api), openEnv, "", "")
	c := check(r, "weights m")
	if !c.Pass || !strings.Contains(c.Detail, weights) {
		t.Fatalf("seeding must pass and name the path: %+v", c)
	}
	if sum.Models[0].WeightsBytes != 2048 {
		t.Errorf("weights bytes = %d, want 2048", sum.Models[0].WeightsBytes)
	}
}

func TestSourcedWeightsStates(t *testing.T) {
	cases := []struct {
		name, status string
		pass         bool
		detail       string
	}{
		{"downloading", `[{"id":"g","state":"downloading","percent":42.5}]`, true, "downloading, 42.5%"},
		{"not wanted yet", `[]`, true, "not wanted yet"},
		{"failed", `[{"id":"g","state":"failed","error":"sha256 mismatch"}]`, false, "sha256 mismatch"},
		{"paused", `[{"id":"g","state":"paused","error":"insufficient disk"}]`, false, "insufficient disk"},
		{"absent", `[{"id":"g","state":"absent"}]`, false, "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r := SummarizeConfig(sourcedConfig(t, "g", fakeParrot(t, tc.status)), openEnv, "", "")
			c := check(r, "weights m")
			if c.Pass != tc.pass || !strings.Contains(c.Detail, tc.detail) {
				t.Errorf("got pass=%v %q, want pass=%v containing %q", c.Pass, c.Detail, tc.pass, tc.detail)
			}
		})
	}
}

func TestSourcedWeightsParrotDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	api := ln.Addr().String()
	ln.Close()
	_, r := SummarizeConfig(sourcedConfig(t, "g", api), openEnv, "", "")
	c := check(r, "weights m")
	if c.Pass || !strings.Contains(c.Detail, "viiwork-parrot at "+api) {
		t.Errorf("an unreachable viiwork-parrot must fail and name its address: %+v", c)
	}
}
