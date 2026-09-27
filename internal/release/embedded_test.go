package release

import "testing"

// A build with no public key compiled in refuses every release, so the tree
// must carry at least one.
func TestAReleaseKeyIsCompiledIn(t *testing.T) {
	keys, err := Keys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Fatal("no release public key in internal/release/keys/")
	}
}
