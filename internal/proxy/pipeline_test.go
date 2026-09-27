package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"text/template"

	"github.com/janit/viiwork/v2/internal/pipeline"
	"github.com/janit/viiwork/v2/meshapi"
)

// testPipeline is a two-step pipeline named "tr": a translation and a JSON QC.
func testPipeline(aliases map[string]string, locales ...string) *pipeline.Pipeline {
	tmpl := template.Must(template.New("p").Parse("Translate into {{.Locale.Language}}."))
	p := &pipeline.Pipeline{Name: "tr", LocaleAliases: aliases, Locales: map[string]*pipeline.LocaleConfig{}}
	for _, l := range locales {
		p.Locales[l] = &pipeline.LocaleConfig{Language: l}
	}
	p.Steps = []pipeline.Step{
		{Name: "translate", Model: "m", Template: tmpl},
		{Name: "qc", Model: "m", Template: tmpl, JSONOutput: true},
	}
	return p
}

func mustPipelineResolver(t *testing.T, pipelines []*pipeline.Pipeline) *PipelineResolver {
	t.Helper()
	pr, err := NewPipelineResolver(pipelines)
	if err != nil {
		t.Fatal(err)
	}
	return pr
}

// A step that names a pipeline's own virtual model would recurse; it is a
// configuration error the node refuses to start on, not a process exit.
func TestPipelineResolverRefusesARecursiveStep(t *testing.T) {
	p := testPipeline(map[string]string{}, "fi")
	p.Steps[1].Model = "tr-fi"
	if _, err := NewPipelineResolver([]*pipeline.Pipeline{p}); err == nil || !strings.Contains(err.Error(), `references virtual model "tr-fi"`) {
		t.Errorf("err = %v", err)
	}
}

func TestPipelineSourceText(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"string", `{"messages":[{"role":"user","content":"Hello"}]}`, "Hello"},
		{"last user message", `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`, "c"},
		{"parts", `{"messages":[{"role":"user","content":[{"type":"text","text":"Hello"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"world"}]}]}`, "Hello\nworld"},
		{"image only", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, ""},
		{"no user", `{"messages":[{"role":"system","content":"x"}]}`, ""},
		{"not json", `{`, ""},
	} {
		if got := pipelineSourceText([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPipelineResolver(t *testing.T) {
	pr := mustPipelineResolver(t, []*pipeline.Pipeline{testPipeline(map[string]string{"br": "pt-BR"}, "fi", "pt-BR")})
	if p, _, key, ok := pr.Resolve("tr-br"); !ok || p.Name != "tr" || key != "pt-BR" {
		t.Errorf("Resolve(tr-br) = %v, %q, %v", p, key, ok)
	}
	if _, _, _, ok := pr.Resolve("m"); ok {
		t.Error("a plain model must not resolve to a pipeline")
	}
	names := pr.VirtualModelNames()
	sort.Strings(names)
	if strings.Join(names, ",") != "tr-br,tr-fi,tr-pt-br" {
		t.Errorf("VirtualModelNames = %v", names)
	}
	for _, e := range pr.VirtualModels() {
		if e.Object != "model" || e.OwnedBy != meshapi.OwnedByPipeline {
			t.Errorf("entry %+v", e)
		}
	}
	if name, ok := pr.MatchesPipelinePrefix("tr-xx"); !ok || name != "tr" {
		t.Errorf("MatchesPipelinePrefix = %q, %v", name, ok)
	}
	locales := pr.AvailableLocales("tr")
	sort.Strings(locales)
	if strings.Join(locales, ",") != "br,fi,pt-br" {
		t.Errorf("AvailableLocales = %v", locales)
	}
}

// pipelineSteps answers every step's call with the same translation; the QC
// step's parse of it fails, which the executor treats as no QC, not an error.
func pipelineSteps(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "Hei maailma"}}},
		})
	}
}

func TestHandlerPipeline(t *testing.T) {
	newFx := func(t *testing.T, status int) *handlerFx {
		f := newHandlerFx(t)
		steps := newRecEngine(t, pipelineSteps(status))
		f.pipelines = mustPipelineResolver(t, []*pipeline.Pipeline{testPipeline(map[string]string{}, "fi")})
		f.exec = pipeline.NewExecutor("http://"+steps.addr(), nil)
		return f.build()
	}

	t.Run("H24 served", func(t *testing.T) {
		f := newFx(t, http.StatusOK)
		rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"tr-fi","messages":[{"role":"user","content":"Hello world"}]}`)
		h := rec.Header()
		if rec.Code != 200 || h.Get("X-Pipeline") != "tr" || h.Get("X-Pipeline-Locale") != "fi" || !strings.Contains(h.Get("X-Pipeline-Steps"), "translate:") || !strings.Contains(h.Get("X-Pipeline-Steps"), "qc:") {
			t.Fatalf("code=%d headers=%v body=%q", rec.Code, h, rec.Body.String())
		}
		var resp struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Object != "chat.completion" || resp.Model != "tr-fi" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hei maailma" || resp.Choices[0].FinishReason != "stop" {
			t.Errorf("response %q: %v", rec.Body.String(), err)
		}
		if len(f.requestEvents()) < 2 {
			t.Errorf("pipeline activity = %+v", f.requestEvents())
		}
	})

	t.Run("no user message", func(t *testing.T) {
		f := newFx(t, http.StatusOK)
		rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"tr-fi","messages":[{"role":"system","content":"x"}]}`)
		if rec.Code != 400 || errorOf(t, rec).Message != "no user message found" {
			t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown locale", func(t *testing.T) {
		f := newFx(t, http.StatusOK)
		rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"tr-xx","messages":[{"role":"user","content":"x"}]}`)
		if rec.Code != 400 || !strings.Contains(errorOf(t, rec).Message, "unknown locale in model 'tr-xx'") {
			t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
		}
	})

	t.Run("step unavailable", func(t *testing.T) {
		f := newFx(t, http.StatusServiceUnavailable)
		rec := f.do(http.MethodPost, "/v1/chat/completions", `{"model":"tr-fi","messages":[{"role":"user","content":"x"}]}`)
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "5" || errorOf(t, rec).Message != "pipeline step 'translate' failed: model 'm' unavailable" {
			t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
		}
	})
}
