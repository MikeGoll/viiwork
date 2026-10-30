package proxy

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/janit/viiwork/v2/internal/clientuse"
	"github.com/janit/viiwork/v2/meshapi"
)

// ledgerMinute is the start of the minute every test request runs in.
const ledgerMinute = int64(1790000040)

// clientFx is a node with one local backend for "m", a member gb1 at
// 127.0.0.1, a ledger on a fixed clock, and the given clients section.
func clientFx(t *testing.T, section map[string][]string) (*handlerFx, *recEngine) {
	t.Helper()
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	f.members = fakeMembers{memberAt("gb1", "127.0.0.1", meshapi.MemberAlive, false)}
	table, err := clientuse.ParseTable(section)
	if err != nil {
		t.Fatal(err)
	}
	f.clients = table
	f.ledger = clientuse.NewLedger(func() time.Time { return time.Unix(ledgerMinute+10, 0) })
	return f.build(), eng
}

func from(addr string) func(*http.Request) {
	return func(r *http.Request) { r.RemoteAddr = addr }
}

func claiming(name string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(meshapi.HeaderClient, name) }
}

// used is the ledger's minute, by client.
func used(f *handlerFx) map[string]clientuse.Usage {
	out := map[string]clientuse.Usage{}
	for _, u := range f.ledger.Take(ledgerMinute) {
		out[u.Client] = u
	}
	return out
}

var section = map[string][]string{"factory-a": {"192.0.2.10"}}

func TestClientIsNamedByAddressNotByHeader(t *testing.T) {
	f, eng := clientFx(t, section)
	rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, from("192.0.2.10:5000"), claiming("batch-b"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := used(f)
	if len(got) != 1 || got["factory-a"].Model != "m" || got["factory-a"].Tokens != 7 {
		t.Errorf("ledger = %+v, want factory-a on m with 7 tokens", got)
	}
	if _, hdr := eng.last(); hdr.Get(meshapi.HeaderClient) != "" {
		t.Errorf("the engine saw %s: %q", meshapi.HeaderClient, hdr.Get(meshapi.HeaderClient))
	}
}

func TestUnknownAddressIsOther(t *testing.T) {
	f, _ := clientFx(t, section)
	f.do(http.MethodPost, "/v1/chat/completions", chatReq, from("192.0.2.99:5000"))
	if got := used(f); len(got) != 1 || got[clientuse.Other].Tokens != 7 {
		t.Errorf("ledger = %+v, want other", got)
	}
}

func TestNoSectionLabelsNothing(t *testing.T) {
	f, eng := clientFx(t, nil)
	rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, claiming("factory-a"))
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if got := used(f); len(got) != 0 {
		t.Errorf("a node with no clients section counted %+v", got)
	}
	if _, hdr := eng.last(); hdr.Get(meshapi.HeaderClient) != "" {
		t.Error("a client's own header reached the engine")
	}
}

func TestVerifiedForwardCarriesItsLabel(t *testing.T) {
	// No section here: a node that only receives forwards needs none.
	f, eng := clientFx(t, nil)
	f.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1"), claiming("factory-a"))
	if got := used(f); len(got) != 1 || got["factory-a"].Tokens != 7 {
		t.Errorf("ledger = %+v, want factory-a", got)
	}
	if _, hdr := eng.last(); hdr.Get(meshapi.HeaderClient) != "" {
		t.Error("the label reached the engine")
	}
}

func TestUnprovenForwardLosesItsLabel(t *testing.T) {
	f, _ := clientFx(t, section)
	// gb9 is not a member, so this is an ordinary client request from
	// 192.0.2.1, the recorder's default source address.
	f.do(http.MethodPost, "/v1/chat/completions", chatReq, claiming("factory-a"), func(r *http.Request) {
		r.Header.Set(meshapi.HeaderForwarded, "gb9")
	})
	if got := used(f); len(got) != 1 || got[clientuse.Other].Tokens != 7 {
		t.Errorf("ledger = %+v, want other", got)
	}
}

func TestForwardWithoutALabel(t *testing.T) {
	// From an older entry node. With a section of its own the receiver
	// records other; with none it records nothing.
	f, _ := clientFx(t, section)
	f.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1"))
	if got := used(f); len(got) != 1 || got[clientuse.Other].Tokens != 7 {
		t.Errorf("with a section: ledger = %+v, want other", got)
	}

	f, _ = clientFx(t, nil)
	f.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1"))
	if got := used(f); len(got) != 0 {
		t.Errorf("without a section: ledger = %+v, want nothing", got)
	}
}

func TestForwardWithABadLabelIsUnlabelled(t *testing.T) {
	f, _ := clientFx(t, nil)
	f.do(http.MethodPost, "/v1/chat/completions", chatReq, fromMember("gb1"), claiming("Not A Name"))
	if got := used(f); len(got) != 0 {
		t.Errorf("ledger = %+v, want nothing", got)
	}
}

// peerFx is an entry node that serves nothing itself and forwards "m" to gb1.
func peerFx(t *testing.T, section map[string][]string) (*handlerFx, *recEngine) {
	t.Helper()
	f := newHandlerFx(t)
	peer := newRecEngine(t, nil)
	f.reports.add("gb1", peer.addr(), time.Now(), peerModel("m", 1, 0))
	table, err := clientuse.ParseTable(section)
	if err != nil {
		t.Fatal(err)
	}
	f.clients = table
	f.ledger = clientuse.NewLedger(func() time.Time { return time.Unix(ledgerMinute+10, 0) })
	return f.build(), peer
}

func TestEntryNodeLabelsItsForward(t *testing.T) {
	f, peer := peerFx(t, section)
	rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, from("192.0.2.10:5000"), claiming("batch-b"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, hdr := peer.last(); hdr.Get(meshapi.HeaderClient) != "factory-a" || hdr.Get(meshapi.HeaderForwarded) != "self" {
		t.Errorf("the peer got %s=%q %s=%q", meshapi.HeaderClient, hdr.Get(meshapi.HeaderClient), meshapi.HeaderForwarded, hdr.Get(meshapi.HeaderForwarded))
	}
	// Time on a peer is the peer's busy time, not this node's.
	if got := used(f); len(got) != 0 {
		t.Errorf("the entry node counted a forward: %+v", got)
	}
}

func TestEntryNodeWithoutASectionSendsNoLabel(t *testing.T) {
	f, peer := peerFx(t, nil)
	f.do(http.MethodPost, "/v1/chat/completions", chatReq, claiming("factory-a"))
	if _, hdr := peer.last(); hdr.Get(meshapi.HeaderClient) != "" {
		t.Errorf("the peer got %s=%q from a node that names nobody", meshapi.HeaderClient, hdr.Get(meshapi.HeaderClient))
	}
}

// Review Focus 3: the first local backend refuses and the second serves. The
// ledger must not read the response capture's token count before a response
// exists, because the capture caches its first answer.
func TestRetryKeepsTheTokenCount(t *testing.T) {
	f := newHandlerFx(t)
	first := true
	eng := newRecEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		if first {
			first = false
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, engineJSON)
	})
	f.local.add("m", eng.addr(), eng.addr()) // two backends, one engine: whichever is tried first refuses
	table, _ := clientuse.ParseTable(section)
	f.clients = table
	f.ledger = clientuse.NewLedger(func() time.Time { return time.Unix(ledgerMinute+10, 0) })
	f.build()

	rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, from("192.0.2.10:5000"))
	if rec.Code != 200 || eng.hits.Load() != 2 {
		t.Fatalf("code %d after %d engine hits, want 200 after 2", rec.Code, eng.hits.Load())
	}
	if got := f.counters.Get("m"); got != (ModelCounters{Requests: 1, Tokens: 7}) {
		t.Errorf("counters = %+v, want one request and 7 tokens", got)
	}
	if got := used(f); got["factory-a"].Tokens != 7 {
		t.Errorf("ledger = %+v, want 7 tokens for factory-a", got)
	}
}

func TestNoLedgerIsFine(t *testing.T) {
	f := newHandlerFx(t)
	eng := newRecEngine(t, nil)
	f.local.add("m", eng.addr())
	table, _ := clientuse.ParseTable(section)
	f.clients = table // an entry node with energy.clients: false still labels
	f.build()
	if rec := f.do(http.MethodPost, "/v1/chat/completions", chatReq, from("192.0.2.10:5000")); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
}
