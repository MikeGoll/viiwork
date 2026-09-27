package joincode

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	for _, c := range []Code{
		{Secret: secret, Seed: "192.0.2.10:7946"},
		{Seed: "192.0.2.10:7946"},
		{Secret: secret, Seed: "[2001:db8::1]:7946"},
	} {
		s, err := Encode(c)
		if err != nil || !strings.HasPrefix(s, "viiwork1-") {
			t.Fatalf("Encode(%+v) = %q, %v", c, s, err)
		}
		got, err := Decode("  " + s + "\n")
		if err != nil || got.Seed != c.Seed || !bytes.Equal(got.Secret, c.Secret) {
			t.Errorf("round trip %+v: %+v, %v", c, got, err)
		}
	}
}

func TestDecodeRefuses(t *testing.T) {
	good, _ := Encode(Code{Secret: bytes.Repeat([]byte{7}, 32), Seed: "192.0.2.10:7946"})
	flip := []byte(good)
	i := len("viiwork1-") + 5
	if flip[i] == 'A' {
		flip[i] = 'B'
	} else {
		flip[i] = 'A'
	}
	cases := map[string]error{
		string(flip):             ErrDamaged,
		good[:len(good)-3]:       ErrDamaged,
		"viiwork2-AAAA":          ErrVersion,
		"hello":                  ErrVersion,
		"viiwork1-!!!not-base64": ErrDamaged,
	}
	for in, want := range cases {
		if _, err := Decode(in); !errors.Is(err, want) {
			t.Errorf("Decode(%q) = %v, want %v", in, err, want)
		}
	}
	if _, err := Encode(Code{Secret: []byte("short"), Seed: "192.0.2.10:7946"}); err == nil {
		t.Error("a short secret was encoded")
	}
	if _, err := Encode(Code{Seed: "node-a.example:7946"}); err == nil {
		t.Error("a host name was accepted as a seed (mesh.seeds needs ip:port)")
	}
}
