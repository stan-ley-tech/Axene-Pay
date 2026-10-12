package webhooks

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func sealer(t *testing.T) *Sealer {
	t.Helper()
	s, err := NewSealer(bytes.Repeat([]byte{7}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealerRoundTripAndBinding(t *testing.T) {
	t.Parallel()
	s := sealer(t)
	blob := s.Seal([]byte("whsec_x"), []byte("whe_A"))
	if pt, err := s.Open(blob, []byte("whe_A")); err != nil || string(pt) != "whsec_x" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if _, err := s.Open(blob, []byte("whe_B")); err == nil {
		t.Fatal("ciphertext opened under another endpoint ID")
	}
	tampered := bytes.Clone(blob)
	tampered[len(tampered)-1] ^= 1
	if _, err := s.Open(tampered, []byte("whe_A")); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	if bytes.Equal(blob, s.Seal([]byte("whsec_x"), []byte("whe_A"))) {
		t.Fatal("nonce reused")
	}
	other, _ := NewSealer(bytes.Repeat([]byte{8}, 32), 1)
	if _, err := other.Open(blob, []byte("whe_A")); err == nil {
		t.Fatal("opened with another key")
	}
	if _, err := NewSealer([]byte("short"), 1); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := s.Open([]byte{1, 2}, nil); err == nil {
		t.Fatal("truncated blob accepted")
	}
}

func TestSignAndVerify(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_790_000_000, 0)
	body := []byte(`{"id":"evt_1"}`)
	h := Sign("whsec_s", now, body)
	if !strings.HasPrefix(h, "t=1790000000,v1=") {
		t.Fatalf("header %q", h)
	}
	if err := Verify("whsec_s", h, body, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	cases := map[string]func() error{
		"wrong secret": func() error { return Verify("whsec_other", h, body, now, 5*time.Minute) },
		"changed body": func() error { return Verify("whsec_s", h, []byte(`{"id":"evt_2"}`), now, 5*time.Minute) },
		"too old":      func() error { return Verify("whsec_s", h, body, now.Add(6*time.Minute), 5*time.Minute) },
		"future":       func() error { return Verify("whsec_s", h, body, now.Add(-6*time.Minute), 5*time.Minute) },
		"malformed":    func() error { return Verify("whsec_s", "garbage", body, now, 5*time.Minute) },
		"no v1":        func() error { return Verify("whsec_s", "t=1790000000", body, now, 5*time.Minute) },
	}
	for name, f := range cases {
		if f() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Multiple v1 values (secret rotation) are accepted if any matches.
	rotated := h + ",v1=" + strings.Repeat("0", 64)
	if err := Verify("whsec_s", "t=1790000000,v1="+strings.Repeat("0", 64)+","+h[len("t=1790000000,"):], body, now, time.Minute); err != nil {
		t.Errorf("rotation: %v (%s)", err, rotated)
	}
}

func TestBlocked(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0", "100.64.0.1",
		"224.0.0.1", "255.255.255.255", "198.18.0.1", "::1", "fc00::1", "fe80::1", "::", "::ffff:127.0.0.1",
		"64:ff9b::a00:1", "ff02::1",
	} {
		if !blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s not blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "41.90.64.1", "2001:4860:4860::8888"} {
		if blocked(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked", s)
		}
	}
}

func TestClientRefusesPrivateAddressesAndRedirects(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/target", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, err := newClient(2*time.Second, false).Post(srv.URL, "application/json", nil)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("loopback allowed: %v", err)
	}
	resp, err := newClient(2*time.Second, true).Post(srv.URL+"/redirect", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("redirect followed: %d", resp.StatusCode)
	}
}

func TestValidateURL(t *testing.T) {
	t.Parallel()
	strict := &Service{requireHTTPS: true}
	dev := &Service{requireHTTPS: false, allowPrivate: true}
	for _, ok := range []string{"https://merchant.example/hooks", "https://merchant.example:8443/a?b=c"} {
		if err := strict.validateURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://merchant.example/hooks", "ftp://merchant.example", "/relative", "https://", "https://user:pw@merchant.example",
		"https://merchant.example/#frag", "https://127.0.0.1/hook", "https://10.0.0.5/hook", "https://localhost/hook",
		"https://[::1]/hook", "https://169.254.169.254/latest", "https://merchant.example/" + strings.Repeat("a", 2048),
	} {
		if err := strict.validateURL(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted", bad)
		}
	}
	if err := dev.validateURL("http://127.0.0.1:9000/hook"); err != nil {
		t.Errorf("development receiver rejected: %v", err)
	}
}

func TestBackoff(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}
	for i, w := range want {
		if got := p.backoff(i); got != w {
			t.Errorf("backoff(%d) = %s, want %s", i, got, w)
		}
	}
	if p.backoff(20) != time.Hour {
		t.Error("backoff not capped")
	}
	total := time.Duration(0)
	for i := 0; i < p.MaxAttempts-1; i++ {
		total += p.backoff(i)
	}
	if total < 8*time.Hour || total > 10*time.Hour {
		t.Errorf("retry window %s outside documented ~9h", total)
	}
}
