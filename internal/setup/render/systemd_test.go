package render

import (
	"strings"
	"testing"
)

func TestEngineUnits(t *testing.T) {
	units, err := EngineUnits("/usr/local/bin/viiwork", "/var/lib/viiwork/releases")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, u := range units {
		byName[u.Name] = string(u.Data)
	}
	for name, want := range map[string][]string{
		"viiwork-engine.service": {"Type=oneshot", "ExecStart=/usr/local/bin/viiwork engine-sync", "After=docker.service"},
		"viiwork-engine.path": {"PathChanged=/var/lib/viiwork/releases/state.json",
			"PathChanged=/var/lib/viiwork/releases/engine-request.json", "Unit=viiwork-engine.service", "WantedBy=multi-user.target"},
		"viiwork-engine.timer": {"OnUnitActiveSec=5min", "Unit=viiwork-engine.service", "WantedBy=timers.target"},
	} {
		got, ok := byName[name]
		if !ok {
			t.Errorf("no %s", name)
		}
		for _, w := range want {
			if !strings.Contains(got, w+"\n") {
				t.Errorf("%s lacks %q:\n%s", name, w, got)
			}
		}
	}
	// systemd splits ExecStart on spaces and expands %: such a path is refused.
	if _, err := EngineUnits("/opt/my viiwork", "/var/lib/viiwork/releases"); err == nil {
		t.Error("a path with a space was rendered")
	}
	if _, err := EngineUnits("/usr/local/bin/viiwork", "/var/lib/100%/releases"); err == nil {
		t.Error("a path with % was rendered")
	}
}
