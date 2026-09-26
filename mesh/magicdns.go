package mesh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// sharedDNSSuffixes are MagicDNS parents that every tailnet shares. A suffix
// equal to one of them names somebody else's machines as much as ours, so it is
// never returned as this tailnet's own.
var sharedDNSSuffixes = map[string]bool{
	"ts.net":             true,
	"tailscale.net":      true,
	"beta.tailscale.net": true,
}

// ErrNoMagicDNSSuffix is returned when tailscaled answers but reports no
// MagicDNS suffix of this tailnet's own.
var ErrNoMagicDNSSuffix = errors.New("tailscaled reports no MagicDNS suffix")

// ReadMagicDNSSuffix asks tailscaled's LocalAPI for this tailnet's MagicDNS
// suffix, such as "tail1234.ts.net": the domain every device of THIS tailnet
// is named under, and no other tailnet's. It is what a browser-origin
// allowlist should trust in place of "*.ts.net", which matches every
// Tailscale customer's Funnel pages too.
//
// It reads the same status document ReadTailnetStatus does, over the same
// unix socket, and is bounded by ctx.
func ReadMagicDNSSuffix(ctx context.Context, socket string) (string, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localAPIStatusURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("tailscaled LocalAPI at %s: %w", socket, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBody))
	if err != nil {
		return "", fmt.Errorf("tailscaled LocalAPI: reading status: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tailscaled LocalAPI answered HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return parseMagicDNSSuffix(body)
}

// parseMagicDNSSuffix takes the suffix from a LocalAPI status document:
// ipnstate.Status.MagicDNSSuffix, else CurrentTailnet.MagicDNSSuffix (both
// untagged Go field names).
func parseMagicDNSSuffix(body []byte) (string, error) {
	var raw struct {
		MagicDNSSuffix string
		CurrentTailnet *struct {
			MagicDNSSuffix string
		}
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("tailscaled LocalAPI: decoding status: %w", err)
	}
	suffix := raw.MagicDNSSuffix
	if suffix == "" && raw.CurrentTailnet != nil {
		suffix = raw.CurrentTailnet.MagicDNSSuffix
	}
	suffix = strings.ToLower(strings.Trim(strings.TrimSpace(suffix), "."))
	if suffix == "" || !strings.Contains(suffix, ".") || sharedDNSSuffixes[suffix] ||
		strings.ContainsAny(suffix, "*/:@ ") {
		return "", ErrNoMagicDNSSuffix
	}
	return suffix, nil
}
