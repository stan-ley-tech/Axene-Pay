package merchant

import (
	"strings"
	"testing"
)

func TestNewSecretFormat(t *testing.T) {
	t.Parallel()
	for _, env := range []Environment{Sandbox, Live} {
		s := newSecret(env)
		got, ok := keyEnvironment(s)
		if !ok || got != env {
			t.Fatalf("keyEnvironment(%q) = %v, %v", s, got, ok)
		}
	}
	if !strings.HasPrefix(newSecret(Live), "sk_live_") || !strings.HasPrefix(newSecret(Sandbox), "sk_test_") {
		t.Fatal("prefix")
	}
	if newSecret(Sandbox) == newSecret(Sandbox) {
		t.Fatal("secrets repeat")
	}
}

func TestKeyEnvironmentRejectsMalformed(t *testing.T) {
	t.Parallel()
	good := newSecret(Sandbox)
	for _, s := range []string{
		"", "sk_test_", "pk_test_" + good[8:], "sk_prod_" + good[8:],
		good + "A", good[:len(good)-1], "sk_test_" + strings.Repeat("!", secretBodyLen),
		"Bearer " + good,
	} {
		if _, ok := keyEnvironment(s); ok {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestHashIsKeyedByPepper(t *testing.T) {
	t.Parallel()
	a := &Service{pepper: []byte(strings.Repeat("a", 32))}
	b := &Service{pepper: []byte(strings.Repeat("b", 32))}
	if string(a.hash("x")) == string(b.hash("x")) {
		t.Fatal("different peppers produced the same hash")
	}
	if len(a.hash("x")) != 32 {
		t.Fatal("hash length")
	}
}

func TestNewServiceRequiresPepper(t *testing.T) {
	t.Parallel()
	if _, err := NewService(nil, []byte("short")); err == nil {
		t.Fatal("short pepper accepted")
	}
}

func TestParseEnvironment(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"sandbox", "live"} {
		if _, err := ParseEnvironment(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "test", "LIVE", "production"} {
		if _, err := ParseEnvironment(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
