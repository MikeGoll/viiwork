package gpu

import (
	"strings"
	"testing"
)

func TestParseVendor(t *testing.T) {
	for _, s := range []string{"nvidia", "amd", "apple", "none"} {
		v, err := ParseVendor(s)
		if err != nil || string(v) != s {
			t.Errorf("ParseVendor(%q) = %q, %v", s, v, err)
		}
	}
	for _, s := range []string{"", "auto", "NVIDIA", "intel"} {
		if _, err := ParseVendor(s); err == nil {
			t.Errorf("ParseVendor(%q) must fail", s)
		}
	}
	if _, err := ParseVendor("intel"); err == nil || !strings.Contains(err.Error(), "apple") {
		t.Errorf("the error must list apple among the choices, got %v", err)
	}
}
