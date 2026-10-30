package node

// A fake Strata server (the kv-export fork's), run as this test binary through
// the model's env (NODE_HELPER=strata), so the node tests run the real Strata
// engine's command line, probes and kv_share calls without a GPU.
//
// It holds one conversation per client session (X-Session-Affinity, else
// X-Session-Id) whose length is the last turn's prompt tokens, and writes it
// as a STRSESS file on /slots/session?action=save. Every chat, save and import
// is appended to FAKE_LOG as one line, so a test can see their order.
//
// Honoured from the model's env:
//
//	FAKE_PROMPT_TOKENS=<n>  prompt tokens in a reply's usage (default 1)
//	FAKE_LOG=<path>         where the lines go
//	FAKE_IMPORT_FAIL=1      every import answers 500

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/janit/viiwork/v2/internal/kvshare/kvsharetest"
)

func serveFakeStrata() {
	args := os.Args[1:]
	wantFlag(args, "--engine", "strata")
	wantFlag(args, "--host", "127.0.0.1")
	raw, err := os.ReadFile(argValue(args, "--config"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake strata:", err)
		os.Exit(1)
	}
	var conf struct {
		ModelName    string   `json:"model_name"`
		Args         []string `json:"args"`
		SlotSavePath string   `json:"slot_save_path"`
	}
	if err := json.Unmarshal(raw, &conf); err != nil {
		fmt.Fprintln(os.Stderr, "fake strata:", err)
		os.Exit(1)
	}
	maxCtx, _ := strconv.Atoi(argValue(conf.Args, "--max-context"))
	tokens, _ := strconv.Atoi(os.Getenv("FAKE_PROMPT_TOKENS"))
	tokens = max(tokens, 1)
	importFail := os.Getenv("FAKE_IMPORT_FAIL") == "1"

	var mu sync.Mutex
	held := map[string]int{}            // session -> conversation length
	lineage := map[string]int{}         // save id -> the length it saved
	nextID := uint64(os.Getpid()) << 32 // ids differ between processes, as the engine's do
	logf := func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(os.Getenv("FAKE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		fmt.Fprintf(f, format+"\n", a...)
		f.Close()
	}
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code int, msg string) {
		reply(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
	}
	slotFile := func(r *http.Request) (string, bool) {
		var body struct{ Session, Filename, Base string }
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Filename == "" || strings.ContainsAny(body.Filename, "/\\") {
			return "", false
		}
		r.Header.Set("X-Fake-Session", body.Session)
		r.Header.Set("X-Fake-Base", body.Base)
		return filepath.Join(conf.SlotSavePath, body.Filename), true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"status": "ok", "max_context": maxCtx, "model": conf.ModelName, "images": false, "api_key": false, "loaded": true, "service": "strata"})
	})
	mux.HandleFunc("GET /slots", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, []map[string]any{{"id": 0, "n_ctx": maxCtx, "is_processing": false}})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, 200, map[string]any{"busy": false, "queued": 0})
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		sess := r.Header.Get("X-Session-Affinity")
		if sess == "" {
			sess = r.Header.Get("X-Session-Id")
		}
		if sess != "" {
			mu.Lock()
			held[sess] = tokens
			mu.Unlock()
		}
		logf("chat %s", sess)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":1,\"total_tokens\":%d,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\n", tokens, tokens+1)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("POST /slots/{slot}", func(w http.ResponseWriter, r *http.Request) {
		path, ok := slotFile(r)
		if !ok {
			fail(w, 400, "filename")
			return
		}
		switch slot, action := r.PathValue("slot"), r.URL.Query().Get("action"); {
		case slot == "session" && action == "save":
			sess := r.Header.Get("X-Fake-Session")
			mu.Lock()
			n, ok := held[sess]
			mu.Unlock()
			if !ok {
				fail(w, 404, "no request of that session on this server")
				return
			}
			conv := func(n int) kvsharetest.Session {
				return kvsharetest.Session{Model: 1, Config: 2, Cells: n, StateSeed: uint64(n)}
			}
			mu.Lock()
			nextID++
			id := nextID
			lineage[fmt.Sprintf("%016x", id)] = n
			baseN, known := lineage[r.Header.Get("X-Fake-Base")]
			mu.Unlock()
			b, kind := conv(n).Sealed(), "save"
			if known {
				baseID, _ := strconv.ParseUint(r.Header.Get("X-Fake-Base"), 16, 64)
				b, kind = conv(n).EngineDelta(conv(baseN), baseID, id), "save-delta"
			}
			if err := os.WriteFile(path, b, 0o600); err != nil {
				fail(w, 500, err.Error())
				return
			}
			logf("%s %s %d", kind, sess, len(b))
			reply(w, 200, map[string]any{"id_slot": 0, "n_saved": n, "n_written": len(b), "save_id": fmt.Sprintf("%016x", id), "delta": known})
		case slot == "0" && action == "import":
			b, err := os.ReadFile(path)
			if err != nil {
				fail(w, 404, "no saved session named that")
				return
			}
			if importFail {
				logf("import-refused %d", len(b))
				fail(w, 500, "fingerprint mismatch")
				return
			}
			logf("import %d", len(b))
			reply(w, 200, map[string]any{"id_slot": 0, "n_read": len(b)})
		default:
			fail(w, 400, "unsupported")
		}
	})
	serveUntilTerm(listenOn(argValue(args, "--port")), mux)
}

// strataModel is a models[] entry served by the fake Strata, with kv_share
// on in group "g". The JSON config and its session folder go in dir.
func strataModel(t interface{ Fatal(...any) }, dir, name string, promptTokens int, log string, env map[string]string) string {
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(dir, "strata.json")
	body := fmt.Sprintf(`{"model_name": %q, "args": ["--max-context", "4096", "--conversation-cache-mib", "64"], "slot_save_path": %q}`, name, sessions)
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  - name: %s\n    engine: strata\n    path: %q\n    gpus: [0]\n    context: 4096\n    parallel: 1\n    startup_timeout: 30s\n", name, conf)
	fmt.Fprintf(&b, "    strata:\n      python: %q\n      dir: /tmp\n      warmup: 0s\n      kv_share:\n        group: g\n        min_tokens: 1000\n", os.Args[0])
	b.WriteString("    env:\n      NODE_HELPER: strata\n")
	fmt.Fprintf(&b, "      FAKE_PROMPT_TOKENS: %q\n      FAKE_LOG: %q\n", strconv.Itoa(promptTokens), log)
	for k, v := range env {
		fmt.Fprintf(&b, "      %s: %q\n", k, v)
	}
	return b.String()
}
