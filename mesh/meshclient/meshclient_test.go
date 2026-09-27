package meshclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewDefaults(t *testing.T) {
	c := New(Options{})
	if c.Timeout != 0 {
		t.Errorf("zero Options must leave the overall timeout unset, got %v", c.Timeout)
	}
	tr := c.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("member traffic must ignore proxy variables")
	}
	if tr.DialContext == nil || tr.MaxIdleConnsPerHost != defaultIdlePerHost || tr.ResponseHeaderTimeout != 0 {
		t.Errorf("transport = %+v", tr)
	}
}

func TestNewOptions(t *testing.T) {
	c := New(Options{Timeout: time.Second, ResponseHeaderTimeout: 2 * time.Second, MaxIdleConnsPerHost: 100})
	tr := c.Transport.(*http.Transport)
	if c.Timeout != time.Second || tr.ResponseHeaderTimeout != 2*time.Second || tr.MaxIdleConnsPerHost != 100 {
		t.Errorf("client = %+v, transport = %+v", c, tr)
	}
}

func TestDialIsBounded(t *testing.T) {
	// 192.0.2.0/24 (TEST-NET-1) is never routed: the dial either fails at once
	// or hangs until the dial timeout, never for the kernel's connect timeout.
	c := New(Options{DialTimeout: 200 * time.Millisecond})
	start := time.Now()
	_, err := c.Get("http://192.0.2.1:9/")
	if err == nil {
		t.Skip("TEST-NET-1 answered; cannot observe the dial timeout here")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("dial took %v, want it bounded by the 200 ms dial timeout", d)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer elsewhere.Close()
	member := httptest.NewServer(http.RedirectHandler(elsewhere.URL, http.StatusFound))
	defer member.Close()

	resp, err := New(Options{}).Get(member.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || followed {
		t.Errorf("status %d, followed %v: a member's redirect must come back, not be followed", resp.StatusCode, followed)
	}
}
