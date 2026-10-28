// Package sessionshare is kv_share on a node: a client session's engine
// state follows it to the member it would spill to, so that a turn which
// cannot run on the session's own host resumes warm on the next one instead
// of prefilling the whole conversation again.
//
// After a local turn of a session, the backend that ran it saves the
// conversation (engine.SessionSharer) and this node sends the file, as a
// delta against what the peer already holds (internal/kvshare), to the member
// of the model's group that SessionRank puts first after itself: the
// session's spill target when this node is its home, and its home when the
// turn spilled here. Before a session's turn runs on a local backend, a
// replica this node holds for it is imported into that backend's cache.
//
// Nothing here is on a client's path but the import, which a spilled turn
// pays once (about 1.5 s at 50k tokens against a 41 s prefill). A failed
// save, push or import leaves a cold prefill, never a wrong answer: the
// engine resumes only from a prompt prefix that matches what it parked.
//
// Design and measurements: docs/superpowers/specs/2026-10-22-kv-share-*.md.
package sessionshare

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/internal/engine"
	"github.com/janit/viiwork/v2/internal/kvshare"
	"github.com/janit/viiwork/v2/internal/logging"
	"github.com/janit/viiwork/v2/internal/route"
	"github.com/janit/viiwork/v2/meshapi"
)

const (
	// defaultSaveDelay lets an agent loop's next turn, which usually follows
	// at once, start before the save that would pause the backend.
	defaultSaveDelay = 2 * time.Second
	// pushTimeout bounds a save and its push: 1 GB at 1 Gbit/s is ~9 s.
	pushTimeout = 5 * time.Minute
	// importTimeout bounds what a spilled turn waits for its import.
	importTimeout = 30 * time.Second
	// heldTimeout bounds asking a backend whether it still holds a session;
	// past it the node trusts its own record, as with an engine that cannot
	// say.
	heldTimeout = 2 * time.Second
	// failLogEvery rate-limits repeated failures per (what, peer).
	failLogEvery = 10 * time.Minute
)

// Backend is a local backend as this package uses it; *supervisor.Backend
// satisfies it.
type Backend interface {
	ID() string
	Addr() string
}

// launcher is *supervisor.Backend's launch time: a relaunched backend has
// lost its cache, so a replica imported before must be imported again. (Not
// its respawn count, which the supervisor resets after a healthy spell.)
type launcher interface{ LaunchedAt() time.Time }

// maxImported bounds a model's record of imports; past it the record starts
// over, which costs at most one needless import per session.
const maxImported = 4096

// maxSessionID is the longest session id Strata keeps a conversation under;
// a longer one is no session to it, so there is nothing to save or resume.
const maxSessionID = 256

func sharable(raw string) bool { return raw != "" && len(raw) <= maxSessionID }

// Model is one configured model that shares sessions.
type Model struct {
	Name   string
	Engine engine.SessionSharer
	Config engine.SessionShareConfig
}

// Deps is what a Manager needs of its node.
type Deps struct {
	Self string
	// Reports are the members' capacity reports (*capacity.Poller).
	Reports    route.Reports
	StaleAfter time.Duration
	// Sign marks and signs a request to a member as a forward (an empty
	// body: a put's file is named by its size and digest in the query).
	Sign func(*http.Request) error
	// Verify reports whether a request is a genuine forward from a member.
	Verify   func(*http.Request) bool
	StateDir string
	Logf     func(format string, args ...any)
	Now      func() time.Time
	// SaveDelay is how long a save waits after its turn; 0 is the default.
	SaveDelay time.Duration
}

// Manager runs kv_share for a node's models.
type Manager struct {
	d      Deps
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	models  map[string]*model
	stores  map[string]*kvshare.Store // by directory, for the process's life
	workers map[string]*worker        // by backend id
	logged  map[string]time.Time
}

type model struct {
	name  string
	eng   engine.SessionSharer
	cfg   engine.SessionShareConfig
	store *kvshare.Store

	pushes, pushFails, bytesSent, puts, imports, importFails atomic.Int64
	usage                                                    atomic.Pointer[kvshare.Usage]
	evicting                                                 atomic.Bool

	mu       sync.Mutex
	imported map[string]string // backend generation + "/" + key -> digest
	costs    []float64         // the last importCosts imports, ms per 1000 tokens
}

// importCosts is how many recent imports the published cost is a median of.
const importCosts = 16

// ImportCost is this node's median time to import a replica into one of the
// model's backends, assembly included, per 1000 tokens; ok is false before
// the first import. Published on /v1/capacity, where the router prices a
// replica host with it.
func (m *Manager) ImportCost(name string) (msPer1k int, ok bool) {
	md := m.model(name)
	if md == nil {
		return 0, false
	}
	md.mu.Lock()
	c := slices.Clone(md.costs)
	md.mu.Unlock()
	if len(c) == 0 {
		return 0, false
	}
	slices.Sort(c)
	return max(int(c[len(c)/2]+0.5), 1), true
}

// New builds a manager with no models.
func New(d Deps) *Manager {
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.SaveDelay <= 0 {
		d.SaveDelay = defaultSaveDelay
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{d: d, ctx: ctx, cancel: cancel, models: map[string]*model{}, stores: map[string]*kvshare.Store{}, workers: map[string]*worker{}, logged: map[string]time.Time{}}
}

// Close stops the save workers and waits for them; a push in progress is
// cancelled.
func (m *Manager) Close() {
	// Under the lock: AfterLocal checks the context and starts a worker
	// under it, so no worker is added once Wait has begun.
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.wg.Wait()
}

// SetModels replaces the models that share sessions, at start and on reload.
// A model whose settings did not change keeps its store and counters. A
// store is cleaned of incomplete replicas when it is first opened.
func (m *Manager) SetModels(models []Model) {
	next := map[string]*model{}
	m.mu.Lock()
	old := m.models
	m.mu.Unlock()
	used := map[string]string{} // store dir -> the model using it
	for _, mm := range models {
		if cur, ok := old[mm.Name]; ok && cur.cfg == mm.Config && cur.eng == mm.Engine {
			next[mm.Name] = cur
			used[cur.store.Dir] = mm.Name
			continue
		}
		dir := mm.Config.StoreDir
		if dir == "" {
			if m.d.StateDir == "" {
				m.d.Logf("kv_share: %s: no state_dir and no kv_share.dir; not sharing its sessions", mm.Name)
				continue
			}
			// Escaped: a name with a slash must not nest in another model's store.
			dir = filepath.Join(m.d.StateDir, "kvshare", url.PathEscape(mm.Name))
		}
		dir = filepath.Clean(dir)
		if other, taken := used[dir]; taken {
			// A store's keys are sessions, which two models share.
			m.d.Logf("kv_share: %s: %s already holds %s's replicas; not sharing its sessions", mm.Name, dir, other)
			continue
		}
		st, u, err := m.openStore(dir)
		if err != nil {
			m.d.Logf("kv_share: %s: %v; not sharing its sessions", mm.Name, err)
			continue
		}
		used[dir] = mm.Name
		md := &model{name: mm.Name, eng: mm.Engine, cfg: mm.Config, store: st, imported: map[string]string{}}
		if cur, ok := old[mm.Name]; ok && cur.store == st {
			// Settings changed, the store did not: what the backends hold
			// and the counters carry over.
			cur.mu.Lock()
			for k, v := range cur.imported {
				md.imported[k] = v
			}
			md.costs = slices.Clone(cur.costs)
			cur.mu.Unlock()
			for _, c := range []struct{ to, from *atomic.Int64 }{
				{&md.pushes, &cur.pushes}, {&md.pushFails, &cur.pushFails}, {&md.bytesSent, &cur.bytesSent},
				{&md.puts, &cur.puts}, {&md.imports, &cur.imports}, {&md.importFails, &cur.importFails},
			} {
				c.to.Store(c.from.Load())
			}
		}
		md.usage.Store(&u)
		next[mm.Name] = md
		m.d.Logf("kv_share: %s: group %q, replicas in %s (%d held, %d MiB), sessions of %d tokens and more", mm.Name, mm.Config.Group, dir, u.Replicas, u.Bytes>>20, mm.Config.MinTokens)
	}
	m.mu.Lock()
	m.models = next
	m.mu.Unlock()
}

// openStore is the store on dir: the one this process already opened, else a
// new one, cleaned once. Two stores on one directory would not share their
// locks, and cleaning a directory in use could remove a put in progress.
func (m *Manager) openStore(dir string) (*kvshare.Store, kvshare.Usage, error) {
	m.mu.Lock()
	st, ok := m.stores[dir]
	m.mu.Unlock()
	if ok {
		u, err := st.Evict(-1)
		return st, u, err
	}
	st = kvshare.NewStore(dir)
	u, err := st.Clean()
	if err != nil {
		return nil, kvshare.Usage{}, err
	}
	m.mu.Lock()
	m.stores[dir] = st
	m.mu.Unlock()
	return st, u, nil
}

func (m *Manager) model(name string) *model {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.models[name]
}

// Group is the model's kv_share group on this node, "" for none: published
// on /v1/capacity so that members push only to members that share.
func (m *Manager) Group(name string) string {
	if md := m.model(name); md != nil {
		return md.cfg.Group
	}
	return ""
}

// Stats is the model's counters, nil when it shares nothing.
func (m *Manager) Stats(name string) *meshapi.KVShareStats {
	md := m.model(name)
	if md == nil {
		return nil
	}
	u := md.usage.Load()
	return &meshapi.KVShareStats{
		Group: md.cfg.Group, Pushes: md.pushes.Load(), PushFailures: md.pushFails.Load(), BytesSent: md.bytesSent.Load(),
		Puts: md.puts.Load(), Imports: md.imports.Load(), ImportFails: md.importFails.Load(),
		Replicas: u.Replicas, StoreBytes: u.Bytes,
	}
}

// storeKey is the file-safe name of a session: its routing hash, so the
// raw header never becomes a file name.
func storeKey(key uint64) string { return fmt.Sprintf("%016x", key) }

// failf logs a failure once per failLogEvery for each (what, peer): one peer
// whose engine refuses every file must not fill the log turn by turn.
func (m *Manager) failf(what, peer, format string, args ...any) {
	k := what + "\x00" + peer
	now := m.d.Now()
	m.mu.Lock()
	last, seen := m.logged[k]
	if seen && now.Sub(last) < failLogEvery {
		m.mu.Unlock()
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: "+format, args...)
		}
		return
	}
	m.logged[k] = now
	m.mu.Unlock()
	m.d.Logf("kv_share: "+format, args...)
}

// target is where a session's replica goes: the first member after this
// node in SessionRank over the fresh members that serve the model in the
// same group with a healthy backend.
func (m *Manager) target(md *model, key uint64) (node, addr string) {
	if m.d.Reports == nil {
		return "", ""
	}
	now := m.d.Now()
	names := []string{m.d.Self}
	addrs := map[string]string{}
	for _, rep := range m.d.Reports.Reports() {
		if rep.Node == m.d.Self || now.Sub(rep.Received) > m.d.StaleAfter {
			continue
		}
		for _, mc := range rep.Models {
			if mc.Name == md.name && mc.KVShareGroup == md.cfg.Group && mc.HealthyBackends > 0 {
				names = append(names, rep.Node)
				addrs[rep.Node] = rep.APIAddr
				break
			}
		}
	}
	for _, n := range route.SessionRank(key, names) {
		if n != m.d.Self {
			return n, addrs[n]
		}
	}
	return "", ""
}

// generation names a backend's current launch.
func generation(b Backend) string {
	if l, ok := b.(launcher); ok {
		return fmt.Sprintf("%s@%d", b.ID(), l.LaunchedAt().UnixNano())
	}
	return b.ID()
}

// BeforeLocal runs before a session's turn goes to local backend b: it drops
// a save still waiting for the session on b (the turn makes it stale), and
// imports this node's replica of the session into b when b has not imported
// that replica yet. It returns when the import is done or has failed, and
// never fails the turn.
func (m *Manager) BeforeLocal(ctx context.Context, modelName, raw string, key uint64, b Backend) {
	if !sharable(raw) {
		return
	}
	m.mu.Lock()
	w := m.workers[b.ID()]
	md := m.models[modelName]
	m.mu.Unlock()
	if w == nil && md == nil {
		return // the common case for a model without kv_share: no allocation
	}
	k := storeKey(key)
	if w != nil {
		w.drop(modelName, k)
	}
	if md == nil {
		return
	}
	rep, ok, err := md.store.Info(k)
	if err != nil || !ok || rep.Tokens < md.cfg.MinTokens {
		return
	}
	gen := generation(b) + "/" + k
	md.mu.Lock()
	had, seen := md.imported[gen]
	md.mu.Unlock()
	if seen && had == rep.Version() && !m.evicted(ctx, md, b, raw, k, rep.Tokens) {
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, importTimeout)
	defer cancel()
	name := "kvi-" + k + ".bin"
	path := filepath.Join(md.cfg.SaveDir, name)
	if _, err := md.store.Materialize(k, path); err != nil {
		md.importFails.Add(1)
		m.failf("materialize", "", "%s: assembling the replica of session %s: %v; serving it cold", modelName, k, err)
		return
	}
	defer os.Remove(path)
	if err := md.eng.ImportSession(ctx, b.Addr(), name); err != nil {
		md.importFails.Add(1)
		m.failf("import", b.ID(), "%s: backend %s refused the replica of session %s (%d tokens): %v; serving it cold", modelName, b.ID(), k, rep.Tokens, err)
		return
	}
	took := time.Since(start)
	md.mu.Lock()
	if len(md.imported) >= maxImported {
		clear(md.imported)
	}
	md.imported[gen] = rep.Version()
	if rep.Tokens >= 1000 {
		md.costs = append(md.costs, float64(took.Milliseconds())*1000/float64(rep.Tokens))
		if len(md.costs) > importCosts {
			md.costs = md.costs[1:]
		}
	}
	md.mu.Unlock()
	md.imports.Add(1)
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: imported session %s (%d tokens, %d MiB) into %s in %d ms", modelName, k, rep.Tokens, rep.Size>>20, b.ID(), took.Milliseconds())
	}
}

// evicted reports that backend b, which was given this replica before, no
// longer holds as much of the conversation: the engine's cache evicts on its
// own and says nothing, so the record of what was imported is only a hint.
// An engine that cannot say keeps the record's answer.
func (m *Manager) evicted(ctx context.Context, md *model, b Backend, raw, k string, tokens int64) bool {
	h, ok := md.eng.(engine.SessionHolder)
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, heldTimeout)
	defer cancel()
	// the replica's own resume point names the conversation: the session's
	// latest branch there may be another one (a compaction on another host)
	n, sum, err := md.store.ResumeIDs(k)
	if err != nil {
		n, sum = 0, 0
	}
	held, err := h.SessionHeld(ctx, b.Addr(), raw, n, sum)
	if err != nil {
		return false
	}
	need := tokens
	if n > 0 {
		need = n // the engine holds what the replica would resume from
	}
	if held >= need {
		return false
	}
	if logging.DebugEnabled() {
		m.d.Logf("[debug] kv_share: %s: backend %s holds %d of the %d tokens it was given; importing again", md.name, b.ID(), held, tokens)
	}
	return true
}

// AfterLocal runs after a session's turn on local backend b completed: a
// conversation of MinTokens or more is saved and pushed to the session's
// replica host, later and in the background (see worker).
func (m *Manager) AfterLocal(modelName, raw string, key uint64, b Backend, promptTokens int64) {
	if !sharable(raw) {
		return
	}
	md := m.model(modelName)
	if md == nil || promptTokens < md.cfg.MinTokens {
		return
	}
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	w := m.workers[b.ID()]
	if w == nil {
		w = newWorker()
		m.workers[b.ID()] = w
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			w.run(m.ctx, m.push)
		}()
	}
	m.mu.Unlock()
	w.add(job{model: modelName, key: storeKey(key), hash: key, raw: raw, backend: b, due: m.d.Now().Add(m.d.SaveDelay)})
}

// push saves one session from its backend and sends it to its replica host.
func (m *Manager) push(ctx context.Context, j job) {
	md := m.model(j.model)
	if md == nil {
		return
	}
	peer, addr := m.target(md, j.hash)
	if peer == "" || addr == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	t := kvshare.NewHTTPTransport(kvshare.HTTPOptions{
		Query: url.Values{"model": {j.model}},
		Sign:  m.d.Sign,
	})
	// What the peer holds first: a peer that cannot answer costs no save
	// (which pauses the backend), and the save id it holds is the base the
	// engine may write a delta against.
	man, err := t.Manifest(ctx, addr, j.key)
	if err != nil {
		md.pushFails.Add(1)
		m.failf("push", peer, "%s: asking %s what it holds of session %s: %v", j.model, peer, j.key, err)
		return
	}
	var base string
	if man != nil {
		base = man.SaveID
	}
	start := time.Now()
	name := "kvs-" + j.key + ".bin"
	path := filepath.Join(md.cfg.SaveDir, name)
	saved, err := md.eng.SaveSession(ctx, j.backend.Addr(), j.raw, name, base)
	if errors.Is(err, engine.ErrSessionGone) {
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: %s: session %s is no longer held by %s; nothing to send", j.model, j.key, j.backend.ID())
		}
		return
	}
	// The engine writes the file; whatever it wrote goes, whatever happens.
	defer os.Remove(path)
	if err != nil {
		md.pushFails.Add(1)
		m.failf("save", j.backend.ID(), "%s: saving session %s from %s: %v", j.model, j.key, j.backend.ID(), err)
		return
	}
	savedAt := time.Now()
	var res kvshare.PushResult
	kind := "diff"
	if saved.Delta {
		kind = "engine delta"
		res, err = kvshare.PushEngineDelta(ctx, t, addr, j.key, path)
	} else {
		res, err = kvshare.PushSave(ctx, t, addr, j.key, path, man, saved.ID)
	}
	if err == nil {
		md.bytesSent.Add(res.Plan.Data)
		md.pushes.Add(1)
		if logging.DebugEnabled() {
			m.d.Logf("[debug] kv_share: %s: session %s to %s (%s): %d of %d MiB in %d ms (save %d ms)", j.model, j.key, peer, kind,
				res.Plan.Data>>20, res.Plan.Size>>20, time.Since(savedAt).Milliseconds(), savedAt.Sub(start).Milliseconds())
		}
		return
	}
	md.pushFails.Add(1)
	m.failf("push", peer, "%s: sending session %s to %s: %v", j.model, j.key, peer, err)
}
