package update

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v2.6.0", "v2.6.0", 0},
		{"v2.6.1", "v2.6.0", 1},
		{"v2.10.0", "v2.9.9", 1},
		{"v3.0.0", "v2.99.99", 1},
		{"v2.6.0", "v2.6.1", -1},
		// scripts/version.sh stamps untagged builds: the same release.
		{"v2.6.0-g1a2b3c4", "v2.6.0", 0},
		{"v2.6.0-g1a2b3c4-dirty", "v2.6.0", 0},
		{"v2.6.0-dirty", "v2.6.0", 0},
		// Real pre-releases keep SemVer order.
		{"v2.6.0-rc.1", "v2.6.0", -1},
		{"v2.6.0-rc.2", "v2.6.0-rc.10", -1},
		{"v2.6.0-beta1", "v2.6.0-rc.1", -1},
		{"v2.6.0-rc.1", "v2.6.0-rc", 1},
		{"v2.6.0-1", "v2.6.0-alpha", -1},
	}
	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if !ok || got != c.want {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d", c.a, c.b, got, ok, c.want)
		}
		if back, _ := Compare(c.b, c.a); back != -c.want {
			t.Errorf("Compare(%q, %q) is not antisymmetric", c.b, c.a)
		}
	}
	for _, bad := range [][2]string{{Builtin, "v2.6.0"}, {"dev", "dev"}, {"v2.6", "v2.6.0"}, {"2.6.0", "v2.6.0"}} {
		if _, ok := Compare(bad[0], bad[1]); ok {
			t.Errorf("Compare(%q, %q) claimed an order", bad[0], bad[1])
		}
	}
}

// A private build of a pre-release (v2.6.0-beta4-g<sha>) is that pre-release,
// as a private build of a release is the release: otherwise it outranks the
// signed beta it was cut from, and a staged beta4 is forgotten as older.
func TestPrivateBuildOfAPreRelease(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v2.6.0-beta4-g1a2b3c4", "v2.6.0-beta4", 0},
		{"v2.6.0-beta4-g1a2b3c4-dirty", "v2.6.0-beta4", 0},
		{"v2.6.0-beta4-dirty", "v2.6.0-beta4", 0},
		{"v2.6.0-beta4-g1a2b3c4", "v2.6.0-beta5", -1},
		{"v2.6.0-alpha.1-g1a2b3c4", "v2.6.0-alpha.2", -1},
		{"v2.6.0-beta4-g1a2b3c4", "v2.6.0", -1},
		{"v2.6.0-g1a2b3c4", "v2.6.0", 0},
	} {
		if got, ok := Compare(c.a, c.b); !ok || got != c.want {
			t.Errorf("Compare(%s, %s) = %d, %v; want %d", c.a, c.b, got, ok, c.want)
		}
	}
}
