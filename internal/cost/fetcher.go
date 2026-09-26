package cost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const minRefetchInterval = 5 * time.Minute

type SpotFetcher struct {
	apiKey  string
	zone    string
	baseURL string
	logger  *log.Logger
	client  *http.Client

	mu          sync.Mutex
	prices      []PricePoint
	lastFetch   time.Time
	lastAttempt time.Time
}

func NewSpotFetcher(apiKey string, zone string, baseURL string) *SpotFetcher {
	return &SpotFetcher{
		apiKey:  apiKey,
		zone:    zone,
		baseURL: baseURL,
		logger:  log.New(os.Stdout, "[spot] ", log.LstdFlags),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (f *SpotFetcher) Fetch(ctx context.Context) {
	if f.client == nil {
		return
	}
	f.mu.Lock()
	if time.Since(f.lastAttempt) < minRefetchInterval {
		f.mu.Unlock()
		return
	}
	f.lastAttempt = time.Now()
	f.mu.Unlock()

	now := time.Now().UTC()
	start := now.Truncate(24 * time.Hour)
	end := start.Add(48 * time.Hour)

	params := url.Values{}
	params.Set("securityToken", f.apiKey)
	params.Set("documentType", "A44")
	params.Set("contract_MarketAgreement.type", "A01")
	params.Set("in_Domain", f.zone)
	params.Set("out_Domain", f.zone)
	params.Set("periodStart", start.Format("200601021504"))
	params.Set("periodEnd", end.Format("200601021504"))

	reqURL := f.baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		f.logger.Printf("failed to create request: %s", f.redactErr(err))
		return
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.logger.Printf("ENTSO-E fetch failed: %s", f.redactErr(err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		f.logger.Printf("ENTSO-E returned %d: %s", resp.StatusCode, f.redact(string(body[:min(len(body), 200)])))
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		f.logger.Printf("failed to read response: %s", f.redactErr(err))
		return
	}

	prices, err := ParsePrices(data)
	if err != nil {
		f.logger.Printf("failed to parse prices: %s", f.redactErr(err))
		return
	}

	f.mu.Lock()
	f.prices = prices
	f.lastFetch = time.Now()
	f.mu.Unlock()

	f.logger.Printf("fetched %d price points", len(prices))
}

// redacted stands in for the API key wherever a message would carry it.
const redacted = "REDACTED"

// redactErr renders a request error without the API key. ENTSO-E takes the key
// as the securityToken query parameter, and a *url.Error's text is the whole
// request URL, so logging a transport failure as-is writes the key to the log.
func (f *SpotFetcher) redactErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return f.redact(fmt.Sprintf("%s %s: %v", ue.Op, redactURL(ue.URL), ue.Err))
	}
	return f.redact(err.Error())
}

// redactURL blanks the securityToken parameter of a request URL. A URL that
// does not parse is dropped whole rather than risked.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	q := u.Query()
	if q.Has("securityToken") {
		q.Set("securityToken", redacted)
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// redact removes the key, raw or query-escaped, from any text: the backstop for
// error types that embed the URL somewhere other than a *url.Error.
func (f *SpotFetcher) redact(s string) string {
	if f.apiKey == "" {
		return s
	}
	s = strings.ReplaceAll(s, f.apiKey, redacted)
	if esc := url.QueryEscape(f.apiKey); esc != f.apiKey {
		s = strings.ReplaceAll(s, esc, redacted)
	}
	return s
}

func (f *SpotFetcher) PriceAt(t time.Time) (float64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.priceAtLocked(t)
}

func (f *SpotFetcher) NeedsFetch(t time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.priceAtLocked(t)
	return !ok
}

func (f *SpotFetcher) priceAtLocked(t time.Time) (float64, bool) {
	t = t.UTC()
	for i, p := range f.prices {
		var next time.Time
		if i+1 < len(f.prices) {
			next = f.prices[i+1].Time
		} else {
			next = p.Time.Add(time.Hour)
		}
		if !t.Before(p.Time) && t.Before(next) {
			return p.CentsKWh, true
		}
	}
	return 0, false
}
