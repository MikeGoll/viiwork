// Package update is a node's side of rolling updates: the release state it
// keeps under state_dir, the startup handover to a staged release, staging,
// confirmation and rollback, and the /v1/update API. docs/releases.md.
package update

import (
	"regexp"
	"strconv"
	"strings"
)

// Builtin names the binary the image or install provides: the floor every
// release state falls back to.
const Builtin = "builtin"

var versionRe = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?$`)

// privateSuffix is what scripts/version.sh appends to an untagged build of a
// release: -g<sha>, -dirty, or both. Such a build IS the release it was cut
// from, not a pre-release of it, or the fleet's own builds would count as
// downgrades from the release they carry.
// It is stripped from the end of any pre-release too: v2.6.0-beta4-g<sha> is
// v2.6.0-beta4.
var privateSuffix = regexp.MustCompile(`(^|-)(g[0-9a-f]{7,40}(-dirty)?|dirty)$`)

type parsed struct {
	n   [3]int
	pre []string // nil for a release
}

func parse(v string) (parsed, bool) {
	m := versionRe.FindStringSubmatch(v)
	if m == nil {
		return parsed{}, false
	}
	var p parsed
	for i := range p.n {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return parsed{}, false
		}
		p.n[i] = n
	}
	if pre := privateSuffix.ReplaceAllString(m[4], ""); pre != "" {
		p.pre = strings.Split(pre, ".")
	}
	return p, true
}

// Compare orders two versions: -1 if a is older than b, 0 if they are the same
// release, +1 if a is newer. ok is false when either is not a version
// (Builtin, "dev").
func Compare(a, b string) (int, bool) {
	pa, okA := parse(a)
	pb, okB := parse(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range pa.n {
		if pa.n[i] != pb.n[i] {
			return sign(pa.n[i] - pb.n[i]), true
		}
	}
	switch {
	case pa.pre == nil && pb.pre == nil:
		return 0, true
	case pa.pre == nil:
		return 1, true
	case pb.pre == nil:
		return -1, true
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := compareIdent(pa.pre[i], pb.pre[i]); c != 0 {
			return c, true
		}
	}
	return sign(len(pa.pre) - len(pb.pre)), true
}

// compareIdent is SemVer's rule for one pre-release identifier: numbers by
// value, below any alphanumeric identifier, which compare as text.
func compareIdent(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return sign(na - nb)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
