package energy

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The client dimension: which client a model's energy was spent on.
//
// It lives in sibling files of its own, so the six rings, models.txt and
// source are exactly what they were and the magic stays VIIWENG1. See
// docs/energy-store-format.md, "Client energy".
const (
	// ClientRecordSize is part of the on-disk format.
	ClientRecordSize = 24
	// DefaultClientLanes is how many (client, model) pairs one bucket holds.
	DefaultClientLanes = 8

	clientMinuteRing = "client-minute.ring"
	clientHourRing   = "client-hour.ring"
	clientsFile      = "clients.txt"

	// clientOther takes the pairs a bucket has no lane for.
	clientOther = "other"
	// maxClientNameLen bounds a line of clients.txt.
	maxClientNameLen = 64
)

// ClientRecord is one time bucket for one (client, model) pair.
type ClientRecord struct {
	TS int64
	// ClientIdx is the row in clients.txt. 0 is nobody: energy the model drew
	// in a bucket where no request was running.
	ClientIdx uint16
	ModelIdx  uint16
	// Wh is energy, not power, so a coarser tier is a plain sum and needs no
	// covered-seconds weight.
	Wh     float32
	BusyS  uint32 // slot-seconds the pair held in the bucket
	Tokens uint32 // completion tokens of requests that ended in it
}

func (r ClientRecord) encode(b []byte) {
	binary.LittleEndian.PutUint64(b[0:], uint64(r.TS))
	binary.LittleEndian.PutUint16(b[8:], r.ClientIdx)
	binary.LittleEndian.PutUint16(b[10:], r.ModelIdx)
	binary.LittleEndian.PutUint32(b[12:], math.Float32bits(r.Wh))
	binary.LittleEndian.PutUint32(b[16:], r.BusyS)
	binary.LittleEndian.PutUint32(b[20:], r.Tokens)
}

func decodeClient(b []byte) ClientRecord {
	return ClientRecord{
		TS:        int64(binary.LittleEndian.Uint64(b[0:])),
		ClientIdx: binary.LittleEndian.Uint16(b[8:]),
		ModelIdx:  binary.LittleEndian.Uint16(b[10:]),
		Wh:        math.Float32frombits(binary.LittleEndian.Uint32(b[12:])),
		BusyS:     binary.LittleEndian.Uint32(b[16:]),
		Tokens:    binary.LittleEndian.Uint32(b[20:]),
	}
}

// ClientUse is one client's use of one model in one minute, as the producer
// measured it: how long the client held a slot, and the tokens it received.
type ClientUse struct {
	Client string
	Model  string
	BusyS  float64
	Tokens uint64
}

// ClientUseFunc reports the use in the minute that starts at bucketStart
// (Unix seconds). The recorder calls it once per finalised minute. It is the
// seam a producer fills, as NodeWattsFunc and GPUReadingsFunc are: this
// package does not know what a request is.
type ClientUseFunc func(bucketStart int64) []ClientUse

// ClientShare is one (client, model) pair's part of a minute. Client "" is
// nobody.
type ClientShare struct {
	Client   string
	ModelIdx uint16
	Wh       float64
	BusyS    float64
	Tokens   uint64
}

// SplitClients divides each model's energy in one minute between the clients
// that held its slots, by busy slot-seconds:
//
//	E(m)    = sum over the model's cards of AttrW * CoveredS / 3600   (Wh)
//	E(c, m) = E(m) * busy(c, m) / busy(m)
//
// A model nobody used in the minute keeps its energy under nobody: loading
// and warming up are not any client's doing. The shares of a model always add
// up to E(m), so with baseline they still equal measured node power. Cards
// with no model (ModelIdx 0) belong to no model and are left out.
//
// Slot-seconds, not tokens: reading a long prompt draws full power and yields
// no completion tokens, and slot time is known for every request on every
// engine.
func SplitClients(gpus []GPURecord, uses []ClientUse, modelIndex func(string) (uint16, error)) ([]ClientShare, error) {
	energy := map[uint16]float64{}
	var models []uint16
	for _, g := range gpus {
		if g.ModelIdx == 0 {
			continue
		}
		if _, ok := energy[g.ModelIdx]; !ok {
			models = append(models, g.ModelIdx)
		}
		energy[g.ModelIdx] += float64(g.AttrW) * float64(g.CoveredS) / 3600
	}

	type pair struct {
		client string
		model  uint16
	}
	busy := map[uint16]float64{}
	merged := map[pair]*ClientShare{}
	var pairs []pair
	for _, u := range uses {
		if u.Client == "" || u.Model == "" {
			continue
		}
		idx, err := modelIndex(u.Model)
		if err != nil {
			return nil, err
		}
		p := pair{u.Client, idx}
		s := merged[p]
		if s == nil {
			s = &ClientShare{Client: u.Client, ModelIdx: idx}
			merged[p] = s
			pairs = append(pairs, p)
		}
		s.BusyS += u.BusyS
		s.Tokens += u.Tokens
		busy[idx] += u.BusyS
	}

	out := make([]ClientShare, 0, len(pairs)+len(models))
	for _, p := range pairs {
		s := *merged[p]
		if b := busy[p.model]; b > 0 {
			s.Wh = energy[p.model] * s.BusyS / b
		}
		out = append(out, s)
	}
	for _, idx := range models {
		if busy[idx] > 0 || energy[idx] == 0 {
			continue
		}
		out = append(out, ClientShare{ModelIdx: idx, Wh: energy[idx]})
	}
	return out, nil
}

// clientStore owns the client files of one store directory.
type clientStore struct {
	mu     sync.Mutex
	dir    string
	lanes  int
	logger *log.Logger
	names  *modelTable // clients.txt has the grammar of models.txt
	minute *ring
	hour   *ring

	totals   clientTotals
	keepFile bool                         // client-totals could not be read or moved aside: never overwrite it
	view     atomic.Pointer[ClientEnergy] // nil = no named client yet

	foldLogged time.Time
}

// openClientStore opens or creates the client files. firstTS is the minute
// that caused them to be created, 0 when they are opened because they exist.
func openClientStore(cfg Config, logger *log.Logger, firstTS int64) (*clientStore, error) {
	cs := &clientStore{dir: cfg.Dir, lanes: cfg.ClientLanes, logger: logger}
	var err error
	if cs.names, err = openModelTable(filepath.Join(cfg.Dir, clientsFile)); err != nil {
		return nil, err
	}
	for _, spec := range []struct {
		target **ring
		name   string
		slots  int
		period int64
	}{
		{&cs.minute, clientMinuteRing, cfg.MinuteSlots, 60},
		{&cs.hour, clientHourRing, cfg.HourSlots, 3600},
	} {
		r, reset, err := openRing(filepath.Join(cfg.Dir, spec.name), spec.slots, cfg.ClientLanes, ClientRecordSize)
		if err != nil {
			cs.close()
			return nil, err
		}
		if reset {
			logger.Printf("%s geometry changed, history discarded", spec.name)
		}
		r.period = spec.period
		*spec.target = r
	}
	if err := cs.loadTotals(firstTS); err != nil {
		cs.close()
		return nil, err
	}
	cs.refresh()
	return cs, nil
}

func (cs *clientStore) rings() []*ring { return []*ring{cs.minute, cs.hour} }

func (cs *clientStore) close() {
	for _, r := range cs.rings() {
		if r != nil {
			r.Close()
		}
	}
}

// validClientName is what a line of clients.txt and of client-totals can
// hold. The node's own rule (internal/clientuse) is stricter; this is the
// format's.
func validClientName(name string) bool {
	return name != "" && name != "-" && len(name) <= maxClientNameLen && !strings.ContainsAny(name, " \t\r\n")
}

// writeMinute records one minute's shares and recomputes its hour.
func (cs *clientStore) writeMinute(ts int64, shares []ClientShare) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	recs := make([]ClientRecord, 0, len(shares))
	for _, sh := range shares {
		if sh.Client != "" && !validClientName(sh.Client) {
			return fmt.Errorf("client name %q cannot be stored", sh.Client)
		}
		idx, err := cs.names.Index(sh.Client)
		if err != nil {
			return err
		}
		recs = append(recs, ClientRecord{
			TS: ts, ClientIdx: idx, ModelIdx: sh.ModelIdx,
			Wh: float32(sh.Wh), BusyS: clampUint32(uint64(sh.BusyS + 0.5)), Tokens: clampUint32(sh.Tokens),
		})
	}
	recs, err := cs.fold(recs, ts)
	if err != nil {
		return err
	}
	if err := cs.minute.putSlot(ts/60, cs.encodeSlot(recs)); err != nil {
		return err
	}
	// Count straight after the ring write: a minute that is in the ring must
	// be in the totals even if the roll-up below fails, or the next minute
	// moves through past it and it is never counted.
	countErr := cs.count(ts, recs)
	rollErr := cs.rollUpHour(ts)
	cs.refresh()
	if countErr != nil {
		return countErr
	}
	return rollErr
}

// fold makes recs fit the lanes. With more pairs than lanes, the lanes-1
// largest by energy keep a lane each and the rest are added into one record
// for the client "other" with no model. No energy is lost; the detail of the
// smallest pairs is.
func (cs *clientStore) fold(recs []ClientRecord, ts int64) ([]ClientRecord, error) {
	if len(recs) <= cs.lanes {
		return recs, nil
	}
	other, err := cs.names.Index(clientOther)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Wh > recs[j].Wh })
	keep := cs.lanes - 1
	var wh float64
	var busy, tokens uint64
	for _, r := range recs[keep:] {
		wh += float64(r.Wh)
		busy += uint64(r.BusyS)
		tokens += uint64(r.Tokens)
	}
	if time.Since(cs.foldLogged) >= time.Hour {
		cs.foldLogged = time.Now()
		cs.logger.Printf("%d client and model pairs in one bucket, %d lanes: the %d smallest are recorded together as %q (raise energy.client_lanes to keep them apart)",
			len(recs), cs.lanes, len(recs)-keep, clientOther)
	}
	rest := ClientRecord{TS: ts, ClientIdx: other, Wh: float32(wh), BusyS: clampUint32(busy), Tokens: clampUint32(tokens)}
	return append(recs[:keep:keep], rest), nil
}

// encodeSlot lays recs out one per lane. Lanes past the last record stay zero.
func (cs *clientStore) encodeSlot(recs []ClientRecord) []byte {
	buf := make([]byte, cs.lanes*ClientRecordSize)
	for i, r := range recs {
		r.encode(buf[i*ClientRecordSize:])
	}
	return buf
}

// rollUpHour rewrites the hour that holds ts from the minute ring. Rewritten,
// not added to, so a repeated write converges, as in the other tiers.
func (cs *clientStore) rollUpHour(ts int64) error {
	start := ts - mod(ts, 3600)
	type pair struct{ client, model uint16 }
	type sum struct {
		wh           float64
		busy, tokens uint64
	}
	sums := map[pair]*sum{}
	var order []pair
	for _, r := range cs.read(cs.minute, start, start+3600) {
		p := pair{r.ClientIdx, r.ModelIdx}
		s := sums[p]
		if s == nil {
			s = &sum{}
			sums[p] = s
			order = append(order, p)
		}
		s.wh += float64(r.Wh)
		s.busy += uint64(r.BusyS)
		s.tokens += uint64(r.Tokens)
	}
	recs := make([]ClientRecord, 0, len(order))
	for _, p := range order {
		s := sums[p]
		recs = append(recs, ClientRecord{TS: start, ClientIdx: p.client, ModelIdx: p.model, Wh: float32(s.wh), BusyS: clampUint32(s.busy), Tokens: clampUint32(s.tokens)})
	}
	recs, err := cs.fold(recs, start)
	if err != nil {
		return err
	}
	return cs.hour.putSlot(start/3600, cs.encodeSlot(recs))
}

// read returns the records of r with TS in [lo, hi), oldest first.
func (cs *clientStore) read(r *ring, lo, hi int64) []ClientRecord {
	buf, err := r.window(lo, hi)
	if err != nil {
		cs.logger.Printf("reading ring: %v", err)
		return nil
	}
	return decodeClients(buf, lo, hi)
}

// readAll returns every record r holds, oldest first.
func (cs *clientStore) readAll(r *ring) []ClientRecord {
	buf, err := r.all()
	if err != nil {
		cs.logger.Printf("reading ring: %v", err)
		return nil
	}
	return decodeClients(buf, math.MinInt64, math.MaxInt64)
}

func decodeClients(buf []byte, lo, hi int64) []ClientRecord {
	var out []ClientRecord
	for off := 0; off+ClientRecordSize <= len(buf); off += ClientRecordSize {
		rec := decodeClient(buf[off : off+ClientRecordSize])
		if rec.TS != 0 && rec.TS >= lo && rec.TS < hi {
			out = append(out, rec)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

func clampUint32(v uint64) uint32 {
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

// HasClients reports whether this store holds client files: a client has been
// named here at some point, and Config.Clients is on.
func (s *Store) HasClients() bool { return s.clients.Load() != nil }

// WriteClientMinute records one finalised minute's client shares, as
// SplitClients produced them, and refreshes the hour it belongs to.
//
// The client files are created by the first write that names a client. Until
// then a store holds none, and a write for nobody alone is dropped: a host
// where nobody named a client is on disk exactly what it was before client
// energy existed. With Config.Clients off this does nothing at all.
func (s *Store) WriteClientMinute(ts int64, shares []ClientShare) error {
	if !s.cfg.Clients {
		return nil
	}
	cs := s.clients.Load()
	if cs == nil {
		named := false
		for _, sh := range shares {
			if sh.Client != "" {
				named = true
				break
			}
		}
		if !named {
			return nil
		}
		var err error
		if cs, err = openClientStore(s.cfg, s.logger, ts); err != nil {
			return err
		}
		s.clients.Store(cs)
	}
	return cs.writeMinute(ts, shares)
}

// ReadClients returns client records in [from, to), oldest first. There is a
// minute tier and an hour tier; the hour ring holds a year, and a record
// carries energy directly, so no day tier is needed.
func (s *Store) ReadClients(tier Tier, from, to time.Time) []ClientRecord {
	cs := s.clients.Load()
	if cs == nil {
		return nil
	}
	switch tier {
	case TierMinute:
		return cs.read(cs.minute, from.Unix(), to.Unix())
	case TierHour:
		return cs.read(cs.hour, from.Unix(), to.Unix())
	}
	return nil
}

// ClientName resolves a stored index back to a client name. Index 0 is "",
// nobody.
func (s *Store) ClientName(idx uint16) string {
	cs := s.clients.Load()
	if cs == nil {
		return ""
	}
	return cs.names.Name(idx)
}
