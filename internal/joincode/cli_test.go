package joincode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

func TestJoinCodeCommand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(meshapi.NodeStatus{Node: "node-a", Addr: "192.0.2.10", APIPort: 8086})
	}))
	defer srv.Close()
	secret := bytes.Repeat([]byte{9}, 32)
	env := func(withSecret bool) Env {
		return Env{
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Client: srv.Client(),
			ReadFile: func(string) ([]byte, error) { return []byte("mesh:\n  bind_port: 7999\n"), nil },
			LookupEnv: func(k string) (string, bool) {
				if withSecret && k == "VIIWORK_MESH_SECRET" {
					return base64.StdEncoding.EncodeToString(secret), true
				}
				return "", false
			},
		}
	}
	node := strings.TrimPrefix(srv.URL, "http://")

	e := env(true)
	if code := Run(context.Background(), []string{"--node", node, "--config", "/etc/viiwork/viiwork.yaml"}, e); code != 0 {
		t.Fatalf("exit %d: %s", code, e.Stderr)
	}
	out := e.Stdout.(*bytes.Buffer).String()
	line := strings.TrimSpace(out[strings.Index(out, "viiwork1-"):])
	line = strings.Fields(line)[0]
	c, err := Decode(line)
	if err != nil || c.Seed != "192.0.2.10:7999" || !bytes.Equal(c.Secret, secret) {
		t.Fatalf("code %q decodes to %+v, %v", line, c, err)
	}
	if !strings.Contains(e.Stderr.(*bytes.Buffer).String(), "secret") {
		t.Error("no warning that the code carries the mesh secret")
	}

	// No secret: an open mesh's code, which says so.
	e = env(false)
	if code := Run(context.Background(), []string{"--node", node, "--open"}, e); code != 0 {
		t.Fatalf("open: exit %d: %s", code, e.Stderr)
	}
	if code := Run(context.Background(), []string{"--node", node}, env(false)); code != 1 {
		t.Error("a secured-mesh code without a secret was printed")
	}
}

// On a Mac the secret lives in the LaunchAgent's environment, not in a
// mesh.env: join-code reads it from the plist when the variable is unset.
func TestJoinCodeReadsTheSecretFromTheLaunchAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(meshapi.NodeStatus{Node: "mac", Addr: "192.0.2.20"})
	}))
	defer srv.Close()
	secret := bytes.Repeat([]byte{7}, 32)
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>fi.viiwork.node</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/usr/bin:/bin</string>
    <key>VIIWORK_MESH_SECRET</key>
    <string>` + base64.StdEncoding.EncodeToString(secret) + `</string>
  </dict>
</dict>
</plist>
`
	var stderr bytes.Buffer
	e := Env{
		Stdout: &bytes.Buffer{}, Stderr: &stderr, Client: srv.Client(),
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile: func(p string) ([]byte, error) {
			if p == "/Users/u/Library/LaunchAgents/fi.viiwork.node.plist" {
				return []byte(plist), nil
			}
			return nil, os.ErrNotExist
		},
		Plist: "/Users/u/Library/LaunchAgents/fi.viiwork.node.plist",
	}
	if code := Run(context.Background(), []string{"--node", strings.TrimPrefix(srv.URL, "http://")}, e); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	out := e.Stdout.(*bytes.Buffer).String()
	c, err := Decode(strings.TrimSpace(out))
	if err != nil || !bytes.Equal(c.Secret, secret) {
		t.Errorf("code %q: %+v, %v", out, c, err)
	}

	// Without the variable or the plist, the error says where the secret is on a Mac.
	stderr.Reset()
	e.ReadFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
	if code := Run(context.Background(), []string{"--node", strings.TrimPrefix(srv.URL, "http://")}, e); code != 1 || !strings.Contains(stderr.String(), "fi.viiwork.node.plist") {
		t.Errorf("exit %d: %s", code, stderr.String())
	}
}
