package ids

import (
	"errors"
	"strings"
	"testing"
)

func TestNewIsWellFormedAndUnique(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		id := New(Payment)
		if _, err := Parse(Payment, id); err != nil {
			t.Fatalf("New produced unparseable %q: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestEncodeKnownValues(t *testing.T) {
	t.Parallel()
	var zero, max [16]byte
	for i := range max {
		max[i] = 0xff
	}
	if got := encode(zero); got != strings.Repeat("0", 26) {
		t.Errorf("zero = %q", got)
	}
	if got := encode(max); got != "7"+strings.Repeat("Z", 25) {
		t.Errorf("max = %q", got)
	}
	one := [16]byte{15: 1}
	if got := encode(one); got != strings.Repeat("0", 25)+"1" {
		t.Errorf("one = %q", got)
	}
	// 2^64 sets the lowest bit of the high word: bit 64 -> char index 26-1-12 holds 2^(64-60)=16 -> 'G'.
	hiBit := [16]byte{7: 1}
	if got := encode(hiBit); got != strings.Repeat("0", 13)+"G"+strings.Repeat("0", 12) {
		t.Errorf("2^64 = %q", got)
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	valid := New(Payment)
	for _, s := range []string{
		"",
		valid[4:],                           // no prefix
		"att_" + valid[4:],                  // wrong prefix
		"pay_" + valid[5:],                  // too short
		valid + "0",                         // too long
		"pay_8" + valid[5:],                 // first char out of range
		"pay_" + valid[4:29] + "I",          // excluded letter
		"pay_" + strings.ToLower(valid[4:]), // lower case
		"PAY_" + valid[4:],
	} {
		if _, err := Parse(Payment, s); !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%q) err = %v, want ErrMalformed", s, err)
		}
	}
	if got, err := Parse(Payment, valid); err != nil || got != valid {
		t.Errorf("Parse(valid) = %q, %v", got, err)
	}
}
