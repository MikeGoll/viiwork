package energy

import (
	"bytes"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustWrite(t *testing.T, s *Store, ts int64, shares ...ClientShare) {
	t.Helper()
	if err := s.WriteClientMinute(ts, shares); err != nil {
		t.Fatal(err)
	}
}

func theClient(t *testing.T, s *Store, name string) ClientTotal {
	t.Helper()
	ce, ok := s.ClientEnergy()
	if !ok {
		t.Fatal("no client energy")
	}
	for _, c := range ce.Clients {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s is not in %+v", name, ce.Clients)
	return ClientTotal{}
}

func TestNoFiguresUntilAClientIsNamed(t *testing.T) {
	s := clientStoreIn(t, t.TempDir(), 8)
	if _, ok := s.ClientEnergy(); ok {
		t.Error("a fresh store publishes client energy")
	}
	mustWrite(t, s, minute0, ClientShare{ModelIdx: 1, Wh: 2})
	if _, ok := s.ClientEnergy(); ok {
		t.Error("a store that only recorded nobody publishes client energy")
	}
}

func TestAMinuteIsCountedOnce(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	first := []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 3, BusyS: 30, Tokens: 10}, {ModelIdx: 2, Wh: 0.5}}
	mustWrite(t, s, minute0, first...)
	mustWrite(t, s, minute0, first...) // again: the ring is rewritten, the total is not added to
	mustWrite(t, s, minute0+60, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 2, BusyS: 30, Tokens: 10})

	ce, ok := s.ClientEnergy()
	if !ok || ce.Since != minute0 || ce.Through != minute0+60 || len(ce.Clients) != 1 {
		t.Fatalf("client energy = %+v, %v", ce, ok)
	}
	c := ce.Clients[0]
	if c.Name != "factory-a" || !near(c.Wh, 5) || c.BusyS != 60 || c.Tokens != 20 {
		t.Errorf("totals: %+v", c)
	}
	// 2 Wh in the last minute is a mean draw of 120 W.
	if !near(c.W1m, 120) || !near(c.KWh24h, 0.005) || !near(c.KWh30d, 0.005) {
		t.Errorf("readings: %+v", c)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "client-totals"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("since %d through %d\n- 0.500 0 0\nfactory-a 5.000 60 20\n", minute0, minute0+60)
	if string(raw) != want {
		t.Errorf("client-totals:\n%s\nwant:\n%s", raw, want)
	}
}

func TestTotalsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	mustWrite(t, s, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3, BusyS: 30, Tokens: 10})
	s.Close()
	s = clientStoreIn(t, dir, 8)
	if c := theClient(t, s, "factory-a"); !near(c.Wh, 3) || c.Tokens != 10 {
		t.Errorf("after reopen: %+v", c)
	}
	mustWrite(t, s, minute0+60, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 1})
	if c := theClient(t, s, "factory-a"); !near(c.Wh, 4) {
		t.Errorf("after one more minute: %+v", c)
	}
}

// A crash between the ring write and the rename: the ring holds a minute the
// totals file does not. Opening adds it.
func TestTotalsCatchUpFromTheRing(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	mustWrite(t, s, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
	path := filepath.Join(dir, "client-totals")
	older, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, s, minute0+60, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 2})
	s.Close()
	if err := os.WriteFile(path, older, 0o644); err != nil { // the rename never happened
		t.Fatal(err)
	}

	s = clientStoreIn(t, dir, 8)
	ce, _ := s.ClientEnergy()
	if c := theClient(t, s, "factory-a"); !near(c.Wh, 5) || ce.Through != minute0+60 || ce.Since != minute0 {
		t.Errorf("caught up to %+v, through %d", c, ce.Through)
	}
	// And only once: a second open adds nothing.
	s.Close()
	s = clientStoreIn(t, dir, 8)
	if c := theClient(t, s, "factory-a"); !near(c.Wh, 5) {
		t.Errorf("a second open counted the minute again: %+v", c)
	}
}

func TestRecreatedStoreHasANewSince(t *testing.T) {
	a := clientStoreIn(t, t.TempDir(), 8)
	mustWrite(t, a, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
	b := clientStoreIn(t, t.TempDir(), 8)
	mustWrite(t, b, minute0+3600, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 1})
	ca, _ := a.ClientEnergy()
	cb, _ := b.ClientEnergy()
	if ca.Since != minute0 || cb.Since != minute0+3600 {
		t.Errorf("since %d and %d", ca.Since, cb.Since)
	}
}

// Review Focus 4: a totals file that cannot be read is moved aside, and the
// store opens and counts again from a new since.
func TestCorruptTotalsAreMovedAside(t *testing.T) {
	for _, damage := range []string{"garbage\n", "since 1 through 2\nfactory-a not-a-number 0 0\n", "since 1 through 2\nnobody-knows-me 1.000 0 0\n", ""} {
		// A store of its own for each kind of damage: a minute written after
		// one repair is in the ring, and the next repair would count it.
		dir := t.TempDir()
		first := clientStoreIn(t, dir, 8)
		mustWrite(t, first, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
		first.Close()
		path := filepath.Join(dir, "client-totals")
		if err := os.WriteFile(path, []byte(damage), 0o644); err != nil {
			t.Fatal(err)
		}
		var logged bytes.Buffer
		before := time.Now().Unix()
		s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, Clients: true}, log.New(&logged, "", 0))
		if err != nil {
			t.Fatalf("%q: the store did not open: %v", damage, err)
		}
		if kept, _ := os.ReadFile(path + ".bad"); string(kept) != damage {
			t.Errorf("%q: client-totals.bad holds %q", damage, kept)
		}
		if !strings.Contains(logged.String(), "client-totals") {
			t.Errorf("%q: nothing was logged: %q", damage, logged.String())
		}
		if _, ok := s.ClientEnergy(); ok {
			t.Errorf("%q: figures survived a file that could not be read", damage)
		}
		// Counting goes on, from now. minute0 is in the past, so the minute
		// the ring still holds is older than the new since and stays out.
		now := time.Now().Unix() / 60 * 60
		if err := s.WriteClientMinute(now, []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 1}}); err != nil {
			t.Fatal(err)
		}
		ce, ok := s.ClientEnergy()
		if !ok || ce.Since < before-60 || !near(ce.Clients[0].Wh, 1) {
			t.Errorf("%q: after the next minute: %+v, %v", damage, ce, ok)
		}
		s.Close()
	}
}

// Review Focus 5: client_lanes is geometry. Changing it discards the two
// rings, out loud, and the cumulative totals survive.
func TestLaneChangeKeepsTheTotals(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	mustWrite(t, s, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3, BusyS: 30, Tokens: 10})
	s.Close()

	var logged bytes.Buffer
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, Clients: true, ClientLanes: 4}, log.New(&logged, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, name := range []string{"client-minute.ring", "client-hour.ring"} {
		if !strings.Contains(logged.String(), name+" geometry changed, history discarded") {
			t.Errorf("the reset of %s was not logged: %q", name, logged.String())
		}
	}
	c := theClient(t, s, "factory-a")
	if !near(c.Wh, 3) || c.BusyS != 30 || c.Tokens != 10 {
		t.Errorf("the totals did not survive: %+v", c)
	}
	if c.KWh24h != 0 || c.W1m != 0 {
		t.Errorf("the rings were discarded, yet: %+v", c)
	}
}

func TestWindowsEndAtThrough(t *testing.T) {
	s := clientStoreIn(t, t.TempDir(), 8)
	mustWrite(t, s, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 1000})
	later := minute0 + 25*3600
	mustWrite(t, s, later, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 500})
	c := theClient(t, s, "factory-a")
	if !near(c.Wh, 1500) || !near(c.KWh24h, 0.5) || !near(c.KWh30d, 1.5) || !near(c.W1m, 30000) {
		t.Errorf("got %+v, want 1500 Wh in all, 0.5 kWh in 24 h, 1.5 kWh in 30 d", c)
	}
}

func TestAtMost32ClientsArePublished(t *testing.T) {
	s, err := Open(Config{Dir: t.TempDir(), GPUIDs: []int{0}, Location: time.UTC, Clients: true, ClientLanes: 64}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var shares []ClientShare
	for i := 1; i <= 33; i++ {
		shares = append(shares, ClientShare{Client: fmt.Sprintf("c%02d", i), ModelIdx: 1, Wh: float64(i)})
	}
	shares = append(shares, ClientShare{ModelIdx: 2, Wh: 99})
	mustWrite(t, s, minute0, shares...)
	ce, _ := s.ClientEnergy()
	if len(ce.Clients) != 32 || ce.Clients[0].Name != "c02" || ce.Clients[31].Name != "c33" {
		t.Errorf("published %d clients, first %q: want the 32 largest, sorted by name, without c01 and without nobody", len(ce.Clients), ce.Clients[0].Name)
	}
}

// A totals problem never costs the node its energy store: with the damaged
// file stuck in place (client-totals.bad is a non-empty directory) Open
// succeeds, the unread file is untouched and minutes are still recorded.
func TestUnmovableTotalsDoNotFailOpen(t *testing.T) {
	dir := t.TempDir()
	first := clientStoreIn(t, dir, 8)
	mustWrite(t, first, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
	first.Close()
	path := filepath.Join(dir, "client-totals")
	if err := os.WriteFile(path, []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "client-totals.bad", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, Clients: true}, log.New(&logged, "", 0))
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()
	if !strings.Contains(logged.String(), "client-totals") {
		t.Errorf("nothing logged: %q", logged.String())
	}
	now := time.Now().Unix() / 60 * 60
	mustWrite(t, s, now, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 1})
	if c := theClient(t, s, "factory-a"); !near(c.Wh, 1) {
		t.Errorf("in-memory totals: %+v", c)
	}
	if kept, _ := os.ReadFile(path); string(kept) != "garbage\n" {
		t.Errorf("the unread file was overwritten: %q", kept)
	}
}

// A totals file that cannot be saved (client-totals.tmp is a non-empty
// directory) does not fail Open during catch-up either.
func TestUnsavableTotalsDoNotFailOpen(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	mustWrite(t, s, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
	path := filepath.Join(dir, "client-totals")
	older, _ := os.ReadFile(path)
	mustWrite(t, s, minute0+60, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 2})
	s.Close()
	if err := os.WriteFile(path, older, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "client-totals.tmp", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, Clients: true}, quiet())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer s.Close()
	if c := theClient(t, s, "factory-a"); !near(c.Wh, 5) {
		t.Errorf("caught up in memory: %+v", c)
	}
}

// What a node publishes before a restart is what it publishes after: memory
// holds the three decimals the file holds, so a counter never steps down.
func TestTotalsNeverStepDownAcrossARestart(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	for i := 0; i < 7; i++ {
		mustWrite(t, s, minute0+int64(i)*60, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 0.1234567, BusyS: 3, Tokens: 1})
	}
	before := theClient(t, s, "factory-a").Wh
	s.Close()
	s = clientStoreIn(t, dir, 8)
	after := theClient(t, s, "factory-a").Wh
	if after < before {
		t.Errorf("wh_total stepped down across a restart: %v -> %v", before, after)
	}
	if after != before {
		t.Errorf("wh_total differs across a restart: %v -> %v", before, after)
	}
}

func TestNonFiniteOrNegativeTotalsAreUnreadable(t *testing.T) {
	for _, wh := range []string{"NaN", "-1", "+Inf", "-Inf", "Inf"} {
		dir := t.TempDir()
		first := clientStoreIn(t, dir, 8)
		mustWrite(t, first, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
		first.Close()
		path := filepath.Join(dir, "client-totals")
		damage := "since 1 through 2\nfactory-a " + wh + " 0 0\n"
		if err := os.WriteFile(path, []byte(damage), 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, Clients: true}, quiet())
		if err != nil {
			t.Fatalf("%s: the store did not open: %v", wh, err)
		}
		if kept, _ := os.ReadFile(path + ".bad"); string(kept) != damage {
			t.Errorf("%s: client-totals.bad holds %q", wh, kept)
		}
		s.Close()
	}
}

func TestNonFiniteShareIsNotCounted(t *testing.T) {
	s := clientStoreIn(t, t.TempDir(), 8)
	mustWrite(t, s, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
	mustWrite(t, s, minute0+60, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: math.NaN(), BusyS: 5})
	mustWrite(t, s, minute0+120, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: -2})
	c := theClient(t, s, "factory-a")
	if math.IsNaN(c.Wh) || math.IsInf(c.Wh, 0) || !near(c.Wh, 3) {
		t.Errorf("wh = %v, want 3", c.Wh)
	}
}

// A ring that cannot be opened costs the node its client dimension, not its
// energy store, and the file is left exactly as it was.
func TestUnopenableClientRingDoesNotFailOpen(t *testing.T) {
	dir := t.TempDir()
	first := clientStoreIn(t, dir, 8)
	mustWrite(t, first, minute0, ClientShare{Client: "factory-a", ModelIdx: 1, Wh: 3})
	first.Close()
	ring := filepath.Join(dir, "client-minute.ring")
	data, err := os.ReadFile(ring)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(ring, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, Clients: true}, quiet())
	if err != nil {
		t.Fatalf("the store did not open: %v", err)
	}
	defer s.Close()
	if s.HasClients() {
		t.Error("client files are open")
	}
	if err := s.WriteMinute(NodeRecord{TS: minute0, Watts: 100, CoveredS: 60}, nil); err != nil {
		t.Errorf("WriteMinute: %v", err)
	}
	// A named write tries again; it fails and leaves the file alone.
	_ = s.WriteClientMinute(minute0+60, []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 1}})
	if got, _ := os.ReadFile(ring); !bytes.Equal(got, data) {
		t.Error("the unreadable ring was changed")
	}
}
