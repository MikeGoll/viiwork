package proxy

import (
	"encoding/json"
	"reflect"
	"testing"
)

// rewriteModelSlow is rewriteModel's decode and re-encode path, the reference
// the splice must agree with.
func rewriteModelSlow(body []byte, model string) []byte {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return body
	}
	name, _ := json.Marshal(model)
	generic["model"] = name
	out, _ := json.Marshal(generic)
	return out
}

func TestRewriteModelSpliceMatchesDecode(t *testing.T) {
	cases := []struct {
		body   string
		splice bool // is the splice expected to take it
	}{
		{`{"model":"stable","messages":[{"role":"user","content":"hi"}]}`, true},
		{`{ "stream" : true , "model" : "stable" , "messages":[]}`, true},
		{`{"temperature":0.2,"model":"sta\"ble\\","max_tokens":5}`, true},
		{"{\n  \"model\": \"stable\",\n  \"messages\": []\n}", true},
		{`{"messages":[{"role":"user","content":"hi"}],"model":"stable"}`, false},     // after a nested value
		{`{"model":"stable","messages":[{"role":"user","content":"\u00e4"}]}`, false}, // an escape anywhere
		{`{"model":"a","model":"stable"}`, false},                                     // duplicate key
		{`{"model":null,"messages":[]}`, false},
		{`{"messages":[{"role":"user","content":"the \"model\" is"}]}`, false},
		{`{"model":"stable","messages":[{"content":"say \"model"}]}`, false},
	}
	for _, c := range cases {
		_, _, ok := modelValueSpan([]byte(c.body))
		if ok != c.splice {
			t.Errorf("%s: splice=%v, want %v", c.body, ok, c.splice)
		}
		for _, model := range []string{"m", `real<&>"model"`} {
			got, want := rewriteModel([]byte(c.body), model), rewriteModelSlow([]byte(c.body), model)
			var gv, wv any
			if err := json.Unmarshal(got, &gv); err != nil {
				t.Errorf("%s: rewritten body does not decode: %v: %s", c.body, err, got)
				continue
			}
			json.Unmarshal(want, &wv)
			if !reflect.DeepEqual(gv, wv) {
				t.Errorf("%s -> %q:\n got %s\nwant %s", c.body, model, got, want)
			}
		}
	}
}

// The splice leaves every other byte alone and never writes to the input.
func TestRewriteModelSpliceKeepsTheRest(t *testing.T) {
	body := []byte(`{ "model" : "stable", "messages":[{"role":"user","content":"hi"}] }`)
	orig := string(body)
	got := rewriteModel(body, "real-model")
	if want := `{ "model" : "real-model", "messages":[{"role":"user","content":"hi"}] }`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if string(body) != orig {
		t.Error("the input body was modified")
	}
}
