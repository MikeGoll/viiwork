package energy

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	clientTotalsFile = "client-totals"
	// maxPublishedClients bounds what ClientEnergy returns, and so what a
	// node puts on its status.
	maxPublishedClients = 32
)

// clientTotal is one client's cumulative counters since clientTotals.since.
type clientTotal struct {
	wh     float64
	busyS  uint64
	tokens uint64
}

// clientTotals is client-totals in memory. through is the start of the last
// minute counted; nothing at or before it is ever added again.
type clientTotals struct {
	since   int64
	through int64
	rows    map[uint16]*clientTotal // by ClientIdx; 0 is nobody
}

// ClientEnergy is a store's energy per named client.
type ClientEnergy struct {
	Since   int64 // when counting began, Unix seconds; changes only if the store is recreated
	Through int64 // start of the last minute counted
	Clients []ClientTotal
}

// ClientTotal is one named client. Wh, BusyS and Tokens are cumulative since
// ClientEnergy.Since and only grow while it stays the same. The other three
// are readings: W1m is the mean draw over the minute at Through, KWh24h and
// KWh30d are sums over the minute ring and the hour ring that end at Through.
type ClientTotal struct {
	Name   string
	Wh     float64
	W1m    float64
	KWh24h float64
	KWh30d float64
	BusyS  uint64
	Tokens uint64
}

// ClientEnergy returns the figures as they stood after the last recorded
// minute. They are rebuilt when a minute is written, not when this is called,
// so calling it every second costs a pointer load. False means no client is
// named in this store.
func (s *Store) ClientEnergy() (ClientEnergy, bool) {
	cs := s.clients.Load()
	if cs == nil {
		return ClientEnergy{}, false
	}
	v := cs.view.Load()
	if v == nil {
		return ClientEnergy{}, false
	}
	return *v, true
}

// loadTotals reads client-totals, or starts one, and then adds every minute
// the ring holds that the file has not counted. A crash between a ring write
// and the rename of the totals therefore loses nothing the ring still has.
//
// A file that cannot be read is moved aside and counting starts again: the
// counters are worth less than the energy store staying open, and a consumer
// sees the new since and starts its own difference again.
func (cs *clientStore) loadTotals(firstTS int64) error {
	path := filepath.Join(cs.dir, clientTotalsFile)
	totals, found, err := readClientTotals(path, cs.names)
	if err != nil {
		bad := path + ".bad"
		if rerr := os.Rename(path, bad); rerr != nil {
			// The unread file stays where it is, and is never written over:
			// the counters run in memory only until the next start.
			cs.logger.Printf("%v; it could not be moved aside (%v), the client counters run in memory only", err, rerr)
			cs.keepFile = true
		} else {
			cs.logger.Printf("%v; moved to %s, the client counters start again", err, bad)
		}
		found = false
	}
	if !found {
		since := firstTS
		if since == 0 {
			since = time.Now().Unix() / 60 * 60
		}
		// through is one minute before since, so the minute that created the
		// file is counted and nothing older in the ring is.
		totals = clientTotals{since: since, through: since - 60, rows: map[uint16]*clientTotal{}}
	}
	cs.totals = totals

	pending := map[int64][]ClientRecord{}
	var minutes []int64
	for _, r := range cs.readAll(cs.minute) { // oldest first
		if r.TS <= cs.totals.through {
			continue
		}
		if _, ok := pending[r.TS]; !ok {
			minutes = append(minutes, r.TS)
		}
		pending[r.TS] = append(pending[r.TS], r)
	}
	for _, ts := range minutes {
		cs.add(ts, pending[ts])
	}
	if !found || len(minutes) > 0 {
		// A totals problem must never cost the node its energy store: log it
		// and carry on in memory; the next minute's count saves again.
		if err := cs.saveTotals(); err != nil {
			cs.logger.Printf("writing %s: %v; the client counters run in memory until it can be written", clientTotalsFile, err)
		}
	}
	return nil
}

// usableWh is whether a record's energy can be counted: a NaN, an infinity or
// a negative number would reach the status and break its encoding.
func usableWh(wh float32) bool {
	f := float64(wh)
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0
}

// roundWh keeps a counter at the precision client-totals is written with, so
// what is published before a restart is what is read back after it.
func roundWh(wh float64) float64 { return math.Round(wh*1000) / 1000 }

func (cs *clientStore) add(ts int64, recs []ClientRecord) {
	for _, r := range recs {
		if !usableWh(r.Wh) {
			continue
		}
		row := cs.totals.rows[r.ClientIdx]
		if row == nil {
			row = &clientTotal{}
			cs.totals.rows[r.ClientIdx] = row
		}
		row.wh = roundWh(row.wh + float64(r.Wh))
		row.busyS += uint64(r.BusyS)
		row.tokens += uint64(r.Tokens)
	}
	cs.totals.through = ts
}

// count adds one written minute to the totals, once.
func (cs *clientStore) count(ts int64, recs []ClientRecord) error {
	if ts <= cs.totals.through {
		return nil
	}
	cs.add(ts, recs)
	return cs.saveTotals()
}

// saveTotals replaces client-totals by writing a new file and renaming it.
func (cs *clientStore) saveTotals() error {
	if cs.keepFile {
		return nil // an unreadable file we could not move aside; see loadTotals
	}
	type line struct {
		name string
		t    *clientTotal
	}
	lines := make([]line, 0, len(cs.totals.rows))
	for idx, t := range cs.totals.rows {
		name := "-" // nobody
		if idx != 0 {
			if name = cs.names.Name(idx); name == "" {
				continue
			}
		}
		lines = append(lines, line{name, t})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].name < lines[j].name })

	var b strings.Builder
	fmt.Fprintf(&b, "since %d through %d\n", cs.totals.since, cs.totals.through)
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %.3f %d %d\n", l.name, l.t.wh, l.t.busyS, l.t.tokens)
	}

	path := filepath.Join(cs.dir, clientTotalsFile)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readClientTotals parses client-totals. found is false for a missing file.
func readClientTotals(path string, names *modelTable) (totals clientTotals, found bool, err error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return clientTotals{}, false, nil
	}
	if err != nil {
		return clientTotals{}, false, fmt.Errorf("reading %s: %w", path, err)
	}
	totals.rows = map[uint16]*clientTotal{}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if _, err := fmt.Sscanf(lines[0], "since %d through %d", &totals.since, &totals.through); err != nil {
		return clientTotals{}, false, fmt.Errorf("%s: first line %q is not \"since N through N\"", path, lines[0])
	}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) != 4 {
			return clientTotals{}, false, fmt.Errorf("%s: line %q does not have four fields", path, l)
		}
		var idx uint16
		if f[0] != "-" {
			i, ok := names.Lookup(f[0])
			if !ok {
				return clientTotals{}, false, fmt.Errorf("%s: %q is not in %s", path, f[0], clientsFile)
			}
			idx = i
		}
		wh, err1 := strconv.ParseFloat(f[1], 64)
		busy, err2 := strconv.ParseUint(f[2], 10, 64)
		tokens, err3 := strconv.ParseUint(f[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return clientTotals{}, false, fmt.Errorf("%s: line %q has a field that is not a number", path, l)
		}
		if math.IsNaN(wh) || math.IsInf(wh, 0) || wh < 0 {
			return clientTotals{}, false, fmt.Errorf("%s: line %q has an energy that is not a finite, non-negative number", path, l)
		}
		totals.rows[idx] = &clientTotal{wh: wh, busyS: busy, tokens: tokens}
	}
	return totals, true, nil
}

// refresh rebuilds the published figures. Called with cs.mu held, or before
// the store is shared.
func (cs *clientStore) refresh() {
	through := cs.totals.through
	end := through + 60
	last, day, month := map[uint16]float64{}, map[uint16]float64{}, map[uint16]float64{}
	for _, r := range cs.read(cs.minute, end-86400, end) {
		if !usableWh(r.Wh) {
			continue
		}
		day[r.ClientIdx] += float64(r.Wh)
		if r.TS == through {
			last[r.ClientIdx] += float64(r.Wh)
		}
	}
	for _, r := range cs.read(cs.hour, end-30*86400, end) {
		if !usableWh(r.Wh) {
			continue
		}
		month[r.ClientIdx] += float64(r.Wh)
	}

	clients := make([]ClientTotal, 0, len(cs.totals.rows))
	for idx, row := range cs.totals.rows {
		if idx == 0 {
			continue // nobody is not a client
		}
		name := cs.names.Name(idx)
		if name == "" {
			continue
		}
		clients = append(clients, ClientTotal{
			Name: name, Wh: row.wh, W1m: last[idx] * 60,
			KWh24h: day[idx] / 1000, KWh30d: month[idx] / 1000,
			BusyS: row.busyS, Tokens: row.tokens,
		})
	}
	if len(clients) == 0 {
		cs.view.Store(nil)
		return
	}
	if len(clients) > maxPublishedClients {
		sort.Slice(clients, func(i, j int) bool {
			if clients[i].Wh != clients[j].Wh {
				return clients[i].Wh > clients[j].Wh
			}
			return clients[i].Name < clients[j].Name
		})
		clients = clients[:maxPublishedClients]
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Name < clients[j].Name })
	cs.view.Store(&ClientEnergy{Since: cs.totals.since, Through: through, Clients: clients})
}
