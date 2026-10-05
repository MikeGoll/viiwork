package render

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/janit/viiwork/v2/internal/update"
)

// Unit is one systemd unit file.
type Unit struct {
	Name string
	Data []byte
}

// EngineUnits are the engine helper's units: a oneshot service that runs
// `viiwork engine-sync`, started when the node changes state.json or asks
// for an image, and every five minutes, which catches anything the path unit
// missed while the service was already running. binary and releases are
// host paths.
func EngineUnits(binary, releases string) ([]Unit, error) {
	for _, p := range []string{binary, releases} {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n\r%\\\"'") {
			return nil, fmt.Errorf("path %q cannot go into a systemd unit: use an absolute path without spaces, quotes or '%%'", p)
		}
	}
	head := "# Written by viiwork init; `viiwork uninstall` removes it.\n"
	service := head + `[Unit]
Description=viiwork engine helper: the compose image follows the node's current release
Documentation=https://github.com/janit/viiwork/blob/main/docs/releases.md
After=docker.service
Wants=docker.service

[Service]
Type=oneshot
ExecStart=` + binary + ` engine-sync
# A first pull of an engine image is gigabytes.
TimeoutStartSec=30min
`
	path := head + `[Unit]
Description=Run the viiwork engine helper when the node's release state changes

[Path]
PathChanged=` + filepath.Join(releases, "state.json") + `
PathChanged=` + filepath.Join(releases, update.ImageRequestFile) + `
Unit=viiwork-engine.service

[Install]
WantedBy=multi-user.target
`
	timer := head + `[Unit]
Description=Run the viiwork engine helper every five minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=5min
Unit=viiwork-engine.service

[Install]
WantedBy=timers.target
`
	return []Unit{
		{"viiwork-engine.service", []byte(service)},
		{"viiwork-engine.path", []byte(path)},
		{"viiwork-engine.timer", []byte(timer)},
	}, nil
}
