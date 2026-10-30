package kvshare

import "github.com/janit/viiwork/v2/internal/kvshare/sesshash"

// SessionHash is Strata's session_hash64 (internal/kvshare/sesshash).
type SessionHash = sesshash.Hash

// NewSessionHash starts a SessionHash with seed.
func NewSessionHash(seed uint64) *SessionHash { return sesshash.New(seed) }

// SessionHash64 is the one-shot form.
func SessionHash64(p []byte, seed uint64) uint64 { return sesshash.Sum(p, seed) }
