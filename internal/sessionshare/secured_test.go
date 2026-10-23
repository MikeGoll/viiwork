package sessionshare_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/kvshare"
	"github.com/janit/viiwork/v2/internal/proxy"
	"github.com/janit/viiwork/v2/internal/sessionshare"
	"github.com/janit/viiwork/v2/mesh"
)

type noMembers struct{}

func (noMembers) Members() []mesh.Member { return nil }

type noEngine struct{}

func (noEngine) SessionShare(engine.Spec) (engine.SessionShareConfig, bool) {
	return engine.SessionShareConfig{}, false
}
func (noEngine) SaveSession(context.Context, string, string, string, string) (engine.SavedSession, error) {
	return engine.SavedSession{}, nil
}
func (noEngine) ImportSession(context.Context, string, string) error { return nil }

// In a secured mesh the receiver takes a replica only with a forward
// signature over the query, which names the file by size and digest.
func TestSecuredMeshNeedsASignature(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	recvAuth, err := proxy.NewForwardAuth("b", secret, nil, noMembers{})
	if err != nil {
		t.Fatal(err)
	}
	sendAuth, err := proxy.NewForwardAuth("a", secret, nil, noMembers{})
	if err != nil {
		t.Fatal(err)
	}
	m := sessionshare.New(sessionshare.Deps{
		Self: "b", StateDir: t.TempDir(), Logf: t.Logf,
		Verify: func(r *http.Request) bool { _, ok := recvAuth.Verify(r, nil); return ok },
	})
	defer m.Close()
	m.SetModels([]sessionshare.Model{{Name: "q", Engine: noEngine{}, Config: engine.SessionShareConfig{
		Group: "g", SaveDir: t.TempDir(), MaxBytes: 1 << 30, MinTokens: 1, MaxFileBytes: 1 << 30,
	}}})
	srv := httptest.NewServer(m)
	defer srv.Close()

	ctx := context.Background()
	q := url.Values{"model": {"q"}}
	unsigned := kvshare.NewHTTPTransport(kvshare.HTTPOptions{Query: q,
		Sign: func(r *http.Request) error { r.Header.Set("X-Viiwork-Forwarded", "a"); return nil }})
	var se *kvshare.StatusError
	if _, err := unsigned.Manifest(ctx, srv.URL, "k"); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("an unsigned claim: %v", err)
	}
	signed := kvshare.NewHTTPTransport(kvshare.HTTPOptions{Query: q,
		Sign: func(r *http.Request) error { return sendAuth.Sign(r, nil) }})
	if man, err := signed.Manifest(ctx, srv.URL, "k"); err != nil || man != nil {
		t.Fatalf("a signed request: %v %v", man, err)
	}
	// a signature does not carry over to another query: the size and digest
	// a put declares are what was signed
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/v1/kvshare/put?model=q&key=k&size=10&digest="+strings.Repeat("00", 32), nil)
	if err := sendAuth.Sign(req, nil); err != nil {
		t.Fatal(err)
	}
	req.URL.RawQuery = strings.Replace(req.URL.RawQuery, "size=10", "size=11", 1)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a changed query kept its signature: %d", resp.StatusCode)
	}
}
