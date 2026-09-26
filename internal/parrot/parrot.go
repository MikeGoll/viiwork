// Package parrot is viiwork's client for the host's viiwork-parrot node, which
// downloads, verifies and seeds model weights so that a host keeps one copy of
// each. It is written against viiwork-parrot's HTTP contract and imports
// nothing from that project: the two release independently.
//
// The API is loopback only and unauthenticated. viiwork-parrot refuses a
// POST without Content-Type: application/json (415) and a request whose Host
// is not loopback (421); the address is always a loopback host:port (config
// validation enforces it), so the Host this client sends is too.
package parrot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout bounds one request. It is independent of how often a caller
// polls.
const DefaultTimeout = 10 * time.Second

// Kind is what an /ensure answer means for the caller.
type Kind int

const (
	// Ready: seeding, every sha256 verified. Path is usable.
	Ready Kind = iota + 1
	// Pending: queued, adopting, downloading or verifying. Ask again.
	Pending
	// Unavailable: no answer, or no catalog yet (503). Ask again later.
	Unavailable
	// Refused: viiwork-parrot will not provide the model, and asking again
	// will not change that.
	Refused
)

func (k Kind) String() string {
	switch k {
	case Ready:
		return "ready"
	case Pending:
		return "pending"
	case Unavailable:
		return "unavailable"
	case Refused:
		return "refused"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Result is one /ensure answer.
type Result struct {
	Kind     Kind
	Path     string  // Ready only: a file, or a directory for a folder model
	State    string  // viiwork-parrot's state word, when it sent a status
	Percent  float64 // 0-100
	DownRate int64   // bytes per second
	Code     int     // HTTP status; 0 when there was no response
	Message  string  // viiwork-parrot's error, or the transport error
}

// ModelStatus is the part of viiwork-parrot's model status viiwork reads.
type ModelStatus struct {
	ID       string  `json:"id"`
	State    string  `json:"state"`
	Percent  float64 `json:"percent"`
	DownRate int64   `json:"down_rate"`
	Path     string  `json:"path,omitempty"`
	Error    string  `json:"error,omitempty"`
}

type ensureResponse struct {
	Path   string          `json:"path"`
	Error  string          `json:"error"`
	Status json.RawMessage `json:"status"`
}

// Client talks to one viiwork-parrot node.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for the viiwork-parrot API at addr (host:port).
func New(addr string) *Client {
	return &Client{base: "http://" + addr, http: &http.Client{Timeout: DefaultTimeout}}
}

// Ensure asks viiwork-parrot to make model id available on this host. It
// never returns an error: every outcome is a Result the caller acts on.
func (c *Client) Ensure(ctx context.Context, id string) Result {
	body, _ := json.Marshal(map[string]string{"id": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/ensure", bytes.NewReader(body))
	if err != nil {
		return Result{Kind: Refused, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{Kind: Unavailable, Message: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var er ensureResponse
	decoded := json.Unmarshal(raw, &er) == nil
	// The status shape is decoded separately and its error ignored: a change
	// to it (or a status that is not an object, e.g. a bare state string)
	// must not stop path and error from being read.
	var status ModelStatus
	_ = json.Unmarshal(er.Status, &status)
	r := Result{
		Code:     resp.StatusCode,
		State:    status.State,
		Percent:  status.Percent,
		DownRate: status.DownRate,
		Message:  er.Error,
	}
	if !decoded {
		// 415 and 421 answer in plain text; keep it, it names the rule.
		r.Message = strings.TrimSpace(string(raw))
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if !decoded {
			// The body isn't the shape we expect at all — retry costs
			// nothing, and treating an unparseable 200 as permanently
			// refused would kill a model whose weights are actually ready.
			r.Kind = Unavailable
			return r
		}
		if er.Path == "" {
			r.Kind, r.Message = Refused, "viiwork-parrot answered 200 without a path"
			return r
		}
		r.Kind, r.Path = Ready, er.Path
	case http.StatusAccepted:
		r.Kind = Pending
	case http.StatusServiceUnavailable:
		r.Kind = Unavailable
		if r.Message == "" {
			r.Message = http.StatusText(resp.StatusCode)
		}
	default:
		r.Kind = Refused
		if r.Message == "" {
			r.Message = http.StatusText(resp.StatusCode)
		}
	}
	return r
}

// Status lists the models viiwork-parrot currently wants on this host. It has
// no side effects, unlike Ensure. A model that is not wanted is not listed,
// so an absent id is either unknown or simply not wanted yet.
func (c *Client) Status(ctx context.Context) ([]ModelStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /status: HTTP %d", resp.StatusCode)
	}
	var out []ModelStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("GET /status: %w", err)
	}
	return out, nil
}
