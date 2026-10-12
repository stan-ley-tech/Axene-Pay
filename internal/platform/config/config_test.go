package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validDev() map[string]string {
	return map[string]string{
		"APP_ENV":      "development",
		"DATABASE_URL": "postgres://u:p@localhost:5432/axene?sslmode=disable",
	}
}

func TestLoadAPIDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := LoadAPI(env(validDev()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.LogLevel != slog.LevelInfo ||
		cfg.ReadTimeout != 10*time.Second || cfg.ShutdownTimeout != 20*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadAPIReportsAllErrors(t *testing.T) {
	t.Parallel()
	_, err := LoadAPI(env(map[string]string{
		"LOG_LEVEL":         "loud",
		"HTTP_READ_TIMEOUT": "-1s",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"APP_ENV", "DATABASE_URL", "LOG_LEVEL", "HTTP_READ_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadAPIRejectsUnknownEnvironment(t *testing.T) {
	t.Parallel()
	m := validDev()
	m["APP_ENV"] = "prod"
	if _, err := LoadAPI(env(m)); err == nil || !strings.Contains(err.Error(), "unknown environment") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeployedEnvironmentsRequireDatabaseTLS(t *testing.T) {
	t.Parallel()
	for _, e := range []string{"sandbox", "production"} {
		for _, tt := range []struct {
			query string
			ok    bool
		}{
			{"", false},
			{"?sslmode=disable", false},
			{"?sslmode=prefer", false},
			{"?sslmode=require", true},
			{"?sslmode=verify-full", true},
		} {
			m := map[string]string{
				"APP_ENV":      e,
				"DATABASE_URL": "postgres://u:p@db.internal:5432/axene" + tt.query,
			}
			_, err := LoadAPI(env(m))
			if (err == nil) != tt.ok {
				t.Errorf("%s %q: err = %v, want ok=%v", e, tt.query, err, tt.ok)
			}
		}
	}
}

func TestDeployedEnvironmentsRejectDebugLogging(t *testing.T) {
	t.Parallel()
	m := map[string]string{
		"APP_ENV":      "production",
		"DATABASE_URL": "postgres://u:p@db:5432/axene?sslmode=require",
		"LOG_LEVEL":    "debug",
	}
	if _, err := LoadAPI(env(m)); err == nil {
		t.Fatal("expected debug logging to be rejected in production")
	}
}

func TestInvalidDatabaseURLDoesNotLeakCredentials(t *testing.T) {
	t.Parallel()
	m := validDev()
	m["DATABASE_URL"] = "mysql://admin:hunter2@db/axene"
	_, err := LoadAPI(env(m))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaks credentials: %v", err)
	}
}

func TestSecretNeverFormats(t *testing.T) {
	t.Parallel()
	s := Secret("hunter2")
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%v %+v %#v %s", s, s, s, s),
		fmt.Sprintf("%+v", API{DatabaseURL: s}),
		s.LogValue().String(),
	} {
		if strings.Contains(out, "hunter2") {
			t.Fatalf("secret leaked: %q", out)
		}
	}
	if s.Reveal() != "hunter2" {
		t.Fatal("Reveal")
	}
}
