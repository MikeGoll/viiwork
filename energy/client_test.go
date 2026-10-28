package energy

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// minute0 is the start of a minute, fourteen minutes into its hour.
const minute0 = int64(1790000040)

var clientFiles = []string{"client-minute.ring", "client-hour.ring", "clients.txt"}

func clientStoreIn(t *testing.T, dir string, lanes int) *Store {
	t.Helper()
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC, Clients: true, ClientLanes: lanes}, quiet())
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func minuteRecords(s *Store, ts int64) []ClientRecord {
	return s.ReadClients(TierMinute, time.Unix(ts, 0), time.Unix(ts+60, 0))
}

func whOf(recs []ClientRecord) float64 {
	var sum float64
	for _, r := range recs {
		sum += float64(r.Wh)
	}
	return sum
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-4*math.Max(1, math.Abs(b)) }

func fixedIndex(names ...string) func(string) (uint16, error) {
	return func(name string) (uint16, error) {
		for i, n := range names {
			if n == name {
				return uint16(i + 1), nil
			}
		}
		return uint16(len(names) + 1), nil
	}
}

func TestClientRecordRoundTrip(t *testing.T) {
	rec := ClientRecord{TS: minute0, ClientIdx: 3, ModelIdx: 2, Wh: 12.75, BusyS: 59, Tokens: 4096}
	buf := make([]byte, ClientRecordSize)
	rec.encode(buf)
	if got := decodeClient(buf); got != rec {
		t.Errorf("round trip: got %+v want %+v", got, rec)
	}
	// The byte layout is the format document's table.
	if buf[8] != 3 || buf[10] != 2 || buf[16] != 59 || buf[20] != 0 || buf[21] != 0x10 {
		t.Errorf("layout: % x", buf)
	}
}

func TestSplitSharesAddUpToTheModelsEnergy(t *testing.T) {
	// Model 1 on two cards: (120 + 180) W for 60 s is 5 Wh. Model 2 on one
	// card: 60 W for 30 covered seconds is 0.5 Wh. Card 3 has no model.
	gpus := []GPURecord{
		{TS: minute0, GPUID: 0, ModelIdx: 1, AttrW: 120, CoveredS: 60},
		{TS: minute0, GPUID: 1, ModelIdx: 1, AttrW: 180, CoveredS: 60},
		{TS: minute0, GPUID: 2, ModelIdx: 2, AttrW: 60, CoveredS: 30},
		{TS: minute0, GPUID: 3, ModelIdx: 0, AttrW: 40, CoveredS: 60},
	}
	uses := []ClientUse{
		{Client: "factory-a", Model: "qwen", BusyS: 45, Tokens: 900},
		{Client: "batch-b", Model: "qwen", BusyS: 15, Tokens: 100},
	}
	shares, err := SplitClients(gpus, uses, fixedIndex("qwen", "granite"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ClientShare{}
	var qwen float64
	for _, s := range shares {
		got[s.Client+"/"+string(rune('0'+s.ModelIdx))] = s
		if s.ModelIdx == 1 {
			qwen += s.Wh
		}
	}
	if len(shares) != 3 {
		t.Fatalf("want three shares, got %+v", shares)
	}
	if a, b := got["factory-a/1"], got["batch-b/1"]; !near(a.Wh, 3.75) || !near(b.Wh, 1.25) || a.Tokens != 900 || b.BusyS != 15 {
		t.Errorf("qwen's split: %+v and %+v, want 3.75 Wh and 1.25 Wh", a, b)
	}
	if !near(qwen, 5) {
		t.Errorf("qwen's shares add up to %v Wh, want 5", qwen)
	}
	// A model nobody used in the minute goes to nobody, not to the clients.
	if n := got["/2"]; n.Client != "" || !near(n.Wh, 0.5) {
		t.Errorf("granite's energy: %+v, want 0.5 Wh for nobody", n)
	}
}

func TestSplitWithNoRequestGoesToNobody(t *testing.T) {
	gpus := []GPURecord{{TS: minute0, GPUID: 0, ModelIdx: 1, AttrW: 60, CoveredS: 60}}
	shares, err := SplitClients(gpus, nil, fixedIndex("qwen"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 || shares[0].Client != "" || shares[0].ModelIdx != 1 || !near(shares[0].Wh, 1) {
		t.Errorf("got %+v, want 1 Wh for nobody", shares)
	}
}

// A model with no card on record (a CPU model, or a card that gave no
// reading) still keeps its client's busy time and tokens, at zero energy.
func TestSplitKeepsUseOfAModelWithNoEnergy(t *testing.T) {
	shares, err := SplitClients(nil, []ClientUse{{Client: "factory-a", Model: "cpu-model", BusyS: 12, Tokens: 7}}, fixedIndex("cpu-model"))
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 || shares[0].Wh != 0 || shares[0].BusyS != 12 || shares[0].Tokens != 7 {
		t.Errorf("got %+v", shares)
	}
}

func TestNoClientFileUntilANamedWrite(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	if err := s.WriteClientMinute(minute0, []ClientShare{{ModelIdx: 1, Wh: 2}}); err != nil {
		t.Fatal(err)
	}
	if s.HasClients() {
		t.Error("a write for nobody opened the client files")
	}
	for _, name := range append(clientFiles, "client-totals") {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists on a store nobody named a client in", name)
		}
	}
	if got := minuteRecords(s, minute0); got != nil {
		t.Errorf("read %+v from a store with no client files", got)
	}
}

func TestClientMinuteSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	shares := []ClientShare{
		{Client: "factory-a", ModelIdx: 1, Wh: 3.75, BusyS: 45, Tokens: 900},
		{Client: "batch-b", ModelIdx: 1, Wh: 1.25, BusyS: 15.4, Tokens: 100},
		{ModelIdx: 2, Wh: 0.5},
	}
	if err := s.WriteClientMinute(minute0, shares); err != nil {
		t.Fatal(err)
	}
	if !s.HasClients() {
		t.Fatal("a named write did not open the client files")
	}
	s.Close()

	s = clientStoreIn(t, dir, 8)
	if !s.HasClients() {
		t.Fatal("a reopened store does not see its client files")
	}
	recs := minuteRecords(s, minute0)
	if len(recs) != 3 || !near(whOf(recs), 5.5) {
		t.Fatalf("read back %+v", recs)
	}
	byName := map[string]ClientRecord{}
	for _, r := range recs {
		byName[s.ClientName(r.ClientIdx)] = r
	}
	if a := byName["factory-a"]; a.TS != minute0 || a.ModelIdx != 1 || a.BusyS != 45 || a.Tokens != 900 {
		t.Errorf("factory-a: %+v", a)
	}
	if b := byName["batch-b"]; b.BusyS != 15 {
		t.Errorf("batch-b's busy seconds are rounded to %d, want 15", b.BusyS)
	}
	if n, ok := byName[""]; !ok || n.ClientIdx != 0 || n.ModelIdx != 2 {
		t.Errorf("nobody: %+v", n)
	}
	// Line 0 of clients.txt is the empty name.
	raw, _ := os.ReadFile(filepath.Join(dir, "clients.txt"))
	if string(raw) != "\nfactory-a\nbatch-b\n" {
		t.Errorf("clients.txt = %q", raw)
	}
}

func TestSecondWriteOfAMinuteChangesNothing(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	shares := []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 3, BusyS: 30, Tokens: 10}}
	if err := s.WriteClientMinute(minute0, shares); err != nil {
		t.Fatal(err)
	}
	before := readFiles(t, dir, clientFiles)
	if err := s.WriteClientMinute(minute0, shares); err != nil {
		t.Fatal(err)
	}
	for name, was := range before {
		if now, _ := os.ReadFile(filepath.Join(dir, name)); !bytes.Equal(was, now) {
			t.Errorf("%s changed on a repeated write", name)
		}
	}
}

func readFiles(t *testing.T, dir string, names []string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

func TestNinePairsInEightLanes(t *testing.T) {
	s := clientStoreIn(t, t.TempDir(), 8)
	var shares []ClientShare
	var total float64
	for i := 1; i <= 9; i++ {
		wh := float64(i) // c1 has the least energy, c9 the most
		shares = append(shares, ClientShare{Client: "c" + string(rune('0'+i)), ModelIdx: 1, Wh: wh, BusyS: 10, Tokens: 100})
		total += wh
	}
	if err := s.WriteClientMinute(minute0, shares); err != nil {
		t.Fatal(err)
	}
	recs := minuteRecords(s, minute0)
	if len(recs) != 8 || !near(whOf(recs), total) {
		t.Fatalf("%d records holding %v Wh, want 8 holding %v", len(recs), whOf(recs), total)
	}
	seen := map[string]ClientRecord{}
	for _, r := range recs {
		seen[s.ClientName(r.ClientIdx)] = r
	}
	if _, kept := seen["c3"]; !kept {
		t.Error("c3, the seventh largest, was not kept")
	}
	if _, kept := seen["c2"]; kept {
		t.Error("c2 kept a lane of its own")
	}
	if o := seen["other"]; !near(float64(o.Wh), 3) || o.ModelIdx != 0 || o.BusyS != 20 || o.Tokens != 200 {
		t.Errorf("the two smallest were not folded into other: %+v", o)
	}
}

func TestUnusedLanesAreZeroAfterALap(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(Config{Dir: dir, GPUIDs: []int{0}, Location: time.UTC, MinuteSlots: 4, Clients: true, ClientLanes: 4}, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	three := []ClientShare{{Client: "a", ModelIdx: 1, Wh: 1}, {Client: "b", ModelIdx: 1, Wh: 2}, {Client: "c", ModelIdx: 1, Wh: 3}}
	if err := s.WriteClientMinute(minute0, three); err != nil {
		t.Fatal(err)
	}
	lap := minute0 + 4*60 // the same slot, one lap later
	if err := s.WriteClientMinute(lap, []ClientShare{{Client: "a", ModelIdx: 1, Wh: 9}}); err != nil {
		t.Fatal(err)
	}
	if recs := minuteRecords(s, lap); len(recs) != 1 || recs[0].Wh != 9 {
		t.Errorf("after a lap: %+v, want one record", recs)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "client-minute.ring"))
	slot := int((lap / 60) % 4)
	lanes := raw[ringHeaderLen+slot*4*ClientRecordSize : ringHeaderLen+(slot+1)*4*ClientRecordSize]
	if !bytes.Equal(lanes[ClientRecordSize:], make([]byte, 3*ClientRecordSize)) {
		t.Errorf("lanes 1 to 3 still hold the previous lap: % x", lanes[ClientRecordSize:])
	}
}

func TestClientHourIsRecomputedFromItsMinutes(t *testing.T) {
	s := clientStoreIn(t, t.TempDir(), 8)
	for i := int64(0); i < 3; i++ {
		shares := []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 2, BusyS: 30, Tokens: 10}, {ModelIdx: 2, Wh: 0.5}}
		if err := s.WriteClientMinute(minute0+60*i, shares); err != nil {
			t.Fatal(err)
		}
	}
	// Writing a minute again must not add to the hour.
	if err := s.WriteClientMinute(minute0, []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 2, BusyS: 30, Tokens: 10}, {ModelIdx: 2, Wh: 0.5}}); err != nil {
		t.Fatal(err)
	}
	hourStart := minute0 - minute0%3600
	hours := s.ReadClients(TierHour, time.Unix(hourStart, 0), time.Unix(hourStart+3600, 0))
	if len(hours) != 2 {
		t.Fatalf("hour records: %+v", hours)
	}
	for _, h := range hours {
		switch s.ClientName(h.ClientIdx) {
		case "factory-a":
			if h.TS != hourStart || !near(float64(h.Wh), 6) || h.BusyS != 90 || h.Tokens != 30 || h.ModelIdx != 1 {
				t.Errorf("factory-a's hour: %+v", h)
			}
		case "":
			if !near(float64(h.Wh), 1.5) || h.ModelIdx != 2 {
				t.Errorf("nobody's hour: %+v", h)
			}
		}
	}
	if s.ReadClients(TierDay, time.Unix(0, 0), time.Unix(minute0+86400, 0)) != nil {
		t.Error("there is no client day tier")
	}
}

// What an older build does with a store that has the new files: it does not
// know them. Clients off is that reader.
func TestClientsOffLeavesTheFilesAlone(t *testing.T) {
	dir := t.TempDir()
	s := clientStoreIn(t, dir, 8)
	if err := s.WriteClientMinute(minute0, []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 3}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := readFiles(t, dir, clientFiles)

	off, err := Open(Config{Dir: dir, GPUIDs: []int{0, 1}, Location: time.UTC}, quiet())
	if err != nil {
		t.Fatalf("a store with client files does not open with clients off: %v", err)
	}
	t.Cleanup(off.Close)
	if off.HasClients() {
		t.Error("clients off opened the client files")
	}
	if err := off.WriteClientMinute(minute0+60, []ClientShare{{Client: "factory-a", ModelIdx: 1, Wh: 3}}); err != nil {
		t.Fatal(err)
	}
	node := NodeRecord{TS: minute0, Watts: 300, CoveredS: 60}
	if err := off.WriteMinute(node, nil); err != nil {
		t.Fatal(err)
	}
	for name, was := range before {
		if now, _ := os.ReadFile(filepath.Join(dir, name)); !bytes.Equal(was, now) {
			t.Errorf("%s changed while clients were off", name)
		}
	}
}

// A store written before client energy existed opens, and gains no file
// until a client is named.
func TestStoreWithoutClientFilesOpens(t *testing.T) {
	dir, _ := legacyStoreDir(t, 2)
	s := clientStoreIn(t, dir, 8)
	if s.HasClients() {
		t.Error("a legacy store has client files")
	}
	for _, name := range clientFiles {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s was created by opening", name)
		}
	}
}

func TestBadClientNameIsRefused(t *testing.T) {
	s := clientStoreIn(t, t.TempDir(), 8)
	for _, bad := range []string{"-", "two words", "line\nbreak"} {
		if err := s.WriteClientMinute(minute0, []ClientShare{{Client: bad, ModelIdx: 1, Wh: 1}}); err == nil {
			t.Errorf("the name %q was stored", bad)
		}
	}
}
