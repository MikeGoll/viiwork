package activity

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/janit/viiwork/v2/meshapi"
)

var nextRequestID atomic.Int64

// NewRequestID returns a unique ID for pairing request start/done events.
func NewRequestID() int64 {
	return nextRequestID.Add(1)
}

// Event is defined in meshapi, the public mesh protocol package, and aliased
// here so this package keeps its familiar name. The type travels on
// /v1/activity/stream and is the sole basis on which every dashboard in the
// fleet reconstructs in-flight requests, so its shape — and the grammar of its
// Message field — is wire contract, not a local logging concern.
type Event = meshapi.Event

// DefaultEventHistory is how many events the ring keeps when nothing is
// configured.
const DefaultEventHistory = 200

const maxSubscribers = 16

type Log struct {
	mu        sync.Mutex
	maxEvents int
	// events is a circular buffer. It grows by append until it holds
	// maxEvents; from then on head is the oldest event's index and each emit
	// overwrites it, so a full ring costs no allocation or copy per event.
	events      []Event
	head        int
	subscribers map[chan []byte]struct{}
	prompts     *PromptStore
}

// NewLog returns a log with the default prompt-history and event-ring
// capacities.
func NewLog() *Log { return NewLogWithHistory(DefaultPromptHistory, DefaultEventHistory) }

// NewLogWithHistory also sizes the event ring (C1 activity.event_history);
// eventHistory <= 0 means DefaultEventHistory. The ring is also how far back a
// reconnecting dashboard can replay, see Backlog.
func NewLogWithHistory(promptHistory, eventHistory int) *Log {
	if eventHistory <= 0 {
		eventHistory = DefaultEventHistory
	}
	return &Log{
		maxEvents:   eventHistory,
		events:      make([]Event, 0, min(eventHistory, DefaultEventHistory)),
		subscribers: make(map[chan []byte]struct{}),
		prompts:     NewPromptStore(promptHistory),
	}
}

// StorePrompt records the prompt text for a request alongside the activity
// log entry for it, so the mesh dashboard can fetch it on demand instead of
// carrying full prompt bodies on every SSE event.
func (l *Log) StorePrompt(rid int64, model, prompt string) {
	l.prompts.Store(rid, time.Now().Unix(), model, prompt)
}

// StoreOutput records the response text for a request once it has finished,
// against the same rid the prompt was stored under.
func (l *Log) StoreOutput(rid int64, model, output string, elapsedMS int64) {
	l.prompts.StoreOutput(rid, time.Now().Unix(), model, output, elapsedMS)
}

// StoreUsage records the reply's token count and generation time for a
// request, after StoreOutput. Zeros mean "not known".
func (l *Log) StoreUsage(rid, outputTokens, genMS int64) {
	l.prompts.StoreUsage(rid, outputTokens, genMS)
}

// PromptHistoryMax reports the prompt store's configured capacity.
func (l *Log) PromptHistoryMax() int { return l.prompts.Max() }

// GetPrompt looks up a previously stored prompt by request id.
func (l *Log) GetPrompt(rid int64) (PromptEntry, bool) {
	return l.prompts.Get(rid)
}

func (l *Log) Emit(typ string, gpuID int, format string, args ...any) {
	l.emit(Event{
		Time:    time.Now().Unix(),
		Type:    typ,
		Message: fmt.Sprintf(format, args...),
		GPUID:   gpuID,
	})
}

func (l *Log) EmitRequest(rid int64, gpuID int, format string, args ...any) {
	l.EmitRequestTask(rid, gpuID, "", format, args...)
}

func (l *Log) EmitRequestTask(rid int64, gpuID int, taskID string, format string, args ...any) {
	l.emit(Event{
		Time:      time.Now().Unix(),
		Type:      "request",
		Message:   fmt.Sprintf(format, args...),
		GPUID:     gpuID,
		RequestID: rid,
		TaskID:    taskID,
	})
}

func (l *Log) emit(ev Event) {
	// Marshalled before the lock: it is the expensive part and needs nothing
	// the lock protects.
	data, _ := json.Marshal(ev)

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.events) < l.maxEvents {
		l.events = append(l.events, ev)
	} else {
		l.events[l.head] = ev
		l.head = (l.head + 1) % l.maxEvents
	}
	// Sent while holding the lock, not to a snapshot taken under it and
	// released first. Subscribe closes the oldest subscriber's channel when it
	// is at capacity, and a send racing that close panics — "send on closed
	// channel", which a select's default case does not prevent. Holding the
	// lock makes closing and sending mutually exclusive. It cannot stall on a
	// slow client, because every send here is non-blocking.
	for ch := range l.subscribers {
		select {
		case ch <- data:
		default: // skip slow clients
		}
	}
}

// ReplayWindow bounds how much history Backlog hands a stream that has just
// opened. A dashboard loading fresh has no use for the whole ring — replaying
// every member's ring made a page load wade through thousands of events —
// only for what happened just now, plus whatever is still running.
const ReplayWindow = 30 * time.Second

// Backlog returns the recent part of the ring marked as replay, for a stream
// that has just opened: every event from the last ReplayWindow, and, from
// before it, the events of requests still in flight.
//
// This is what makes a dropped connection recoverable. A consumer
// reconstructing in-flight requests from start/done pairs loses the pairing for
// anything that completes while it is away — a laptop sleeping, a tab throttled
// in the background, a node restarting — and a start with no matching done
// strands a row that never leaves. Replaying hands back both halves. The
// in-flight exception is why the window cannot simply cut: an inference running
// for minutes started long before it, and dropping its start would make it
// vanish from a reloaded dashboard while it is still running.
//
// The ring bounds how far back that works. A gap longer than the ring on a
// given node cannot be repaired from here, which is why a consumer should also
// treat a reconnect as a reason to rebuild rather than to carry state across.
func (l *Log) Backlog() []Event { return l.backlogAt(time.Now()) }

func (l *Log) backlogAt(now time.Time) []Event {
	cutoff := now.Add(-ReplayWindow).Unix()
	all := l.Recent()

	// A request whose terminal event is in the ring is over, whenever it began.
	var ended map[int64]bool
	for _, ev := range all {
		if ev.RequestID != 0 && meshapi.IsRequestTerminal(ev.Message) {
			if ended == nil {
				ended = make(map[int64]bool)
			}
			ended[ev.RequestID] = true
		}
	}

	out := all[:0]
	for _, ev := range all {
		if ev.Time < cutoff && (ev.RequestID == 0 || ended[ev.RequestID]) {
			continue
		}
		ev.Replay = true
		out = append(out, ev)
	}
	return out
}

func (l *Log) Recent() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Oldest first: the part from head to the end, then the wrapped part.
	out := make([]Event, len(l.events))
	n := copy(out, l.events[l.head:])
	copy(out[n:], l.events[:l.head])
	return out
}

func (l *Log) Subscribe() chan []byte {
	ch := make(chan []byte, 32)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.subscribers) >= maxSubscribers {
		// Evict one to make room. Which one is arbitrary: subscribers are a
		// map and Go randomises its iteration order, so this is not "the
		// oldest" however much that would be nicer. It does not matter for
		// correctness — the evicted reader sees its channel close and returns,
		// and a dashboard reconnects — but it is worth not mistaking for an
		// ordering guarantee.
		for evict := range l.subscribers {
			delete(l.subscribers, evict)
			close(evict)
			break
		}
	}
	l.subscribers[ch] = struct{}{}
	return ch
}

func (l *Log) Unsubscribe(ch chan []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.subscribers, ch)
}
