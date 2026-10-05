package alias

import (
	"errors"
	"reflect"
	"testing"

	"github.com/janit/viiwork/v2/meshapi"
)

// Broadcast and push/pull apply one rule to a remote entry: a live entry
// without a target, or one past MaxAliasVer, changes nothing on either path.
func TestMergeRefusesInvalidRemoteEntries(t *testing.T) {
	cases := map[string]meshapi.AliasEntry{
		"live entry with no target": E(9, 100, "node-a", ""),
		"version past MaxAliasVer":  E(MaxAliasVer+1, 100, "node-a", "B"),
		"version at MaxUint64":      E(^uint64(0), 100, "node-a", "B"),
	}
	for label, remote := range cases {
		t.Run(label+" broadcast", func(t *testing.T) {
			s, writes, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(1, 50, "gb1", "A")})
			changed, conflict := mustMerge(t, s, "a", remote)
			mustMerge(t, s, "new", remote)
			got, _ := s.Get("a")
			if _, stored := s.Get("new"); changed || conflict != nil || got.Target != "A" || stored || *writes != 0 {
				t.Errorf("changed=%v conflict=%v entry=%+v stored=%v writes=%d", changed, conflict, got, stored, *writes)
			}
		})
		t.Run(label+" push/pull", func(t *testing.T) {
			s, writes, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(1, 50, "gb1", "A")})
			changed, conflicts, err := s.MergeTable(meshapi.AliasTable{V: 1, Aliases: map[string]meshapi.AliasEntry{
				"a": remote, "new": remote, "ok": E(1, 100, "node-a", "C"),
			}})
			got, _ := s.Get("a")
			_, stored := s.Get("new")
			if err != nil || !reflect.DeepEqual(changed, []string{"ok"}) || len(conflicts) != 0 || got.Target != "A" || stored || *writes != 1 {
				t.Errorf("changed=%v conflicts=%v err=%v entry=%+v stored=%v writes=%d", changed, conflicts, err, got, stored, *writes)
			}
		})
	}

	t.Run("version exactly MaxAliasVer is accepted", func(t *testing.T) {
		s, _, _ := mergeStore(t, nil)
		if changed, _ := mustMerge(t, s, "a", E(MaxAliasVer, 100, "node-a", "B")); !changed {
			t.Error("an entry at MaxAliasVer was refused")
		}
	})

	t.Run("a tombstone with no target is accepted by push/pull", func(t *testing.T) {
		s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": E(1, 50, "gb1", "A")})
		tomb := E(2, 100, "node-a", "")
		tomb.Deleted = true
		_, _, err := s.MergeTable(meshapi.AliasTable{V: 1, Aliases: map[string]meshapi.AliasEntry{"a": tomb}})
		if got, _ := s.Get("a"); err != nil || !got.Deleted {
			t.Errorf("err=%v entry=%+v", err, got)
		}
	})
}

// A local write never takes an alias past MaxAliasVer, so the counter cannot
// wrap.
func TestLocalWriteAtMaxAliasVer(t *testing.T) {
	s, _, _ := mergeStore(t, map[string]meshapi.AliasEntry{"a": withHistory(E(MaxAliasVer, 50, "gb1", "A"), V(10, "gb1", "old"))})
	if _, err := s.Set("a", "B", nil); !errors.Is(err, ErrVersionExhausted) {
		t.Errorf("Set: err = %v, want ErrVersionExhausted", err)
	}
	if _, err := s.Delete("a"); !errors.Is(err, ErrVersionExhausted) {
		t.Errorf("Delete: err = %v, want ErrVersionExhausted", err)
	}
	if _, err := s.Revert("a"); !errors.Is(err, ErrVersionExhausted) {
		t.Errorf("Revert: err = %v, want ErrVersionExhausted", err)
	}
	if got, _ := s.Get("a"); got.Ver != MaxAliasVer || got.Target != "A" {
		t.Errorf("entry = %+v", got)
	}

	s, _, _ = mergeStore(t, map[string]meshapi.AliasEntry{"a": E(MaxAliasVer-1, 50, "gb1", "A")})
	if e, err := s.Set("a", "B", nil); err != nil || e.Ver != MaxAliasVer {
		t.Errorf("Set: entry=%+v err=%v", e, err)
	}
}
