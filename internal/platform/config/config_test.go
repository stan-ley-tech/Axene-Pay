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
		"APP_ENV":             "development",
		"DATABASE_URL":        "postgres://u:p@localhost:5432/axene?sslmode=disable",
		"API_KEY_PEPPER":      strings.Repeat("p", 32),
		"PUBLIC_BASE_URL":     "http://localhost:8080/",
		"CALLBACK_TOKEN":      strings.Repeat("t", 32),
		"RABBITMQ_URL":        "amqp://u:p@localhost:5672/",
		"WEBHOOK_SECRETS_KEY": "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
	}
}

func validProd() map[string]string {
	m := validDev()
	m["APP_ENV"] = "production"
	m["DATABASE_URL"] = "postgres://u:p@db:5432/axene?sslmode=require"
	m["PUBLIC_BASE_URL"] = "https://api.example.com"
	m["RABBITMQ_URL"] = "amqps://u:p@mq:5671/"
	return m
}

func with(m map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestLoadAPIDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := LoadAPI(env(validDev()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.LogLevel != slog.LevelInfo || cfg.MetricsAddr != ":9090" ||
		cfg.ReadTimeout != 10*time.Second || cfg.ProviderTimeout != 30*time.Second || cfg.TestProviderEnabled {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.WebhookSecretsKey.Reveal()) != 32 || cfg.WebhookAllowPrivateTargets {
		t.Fatalf("webhook config: %d bytes, private=%v", len(cfg.WebhookSecretsKey.Reveal()), cfg.WebhookAllowPrivateTargets)
	}
	if got := cfg.CallbackURL("testprovider"); got != "http://localhost:8080/callbacks/testprovider/"+strings.Repeat("t", 32) {
		t.Fatalf("CallbackURL = %q", got)
	}
}

func TestLoadWorker(t *testing.T) {
	t.Parallel()
	cfg, err := LoadWorker(env(validDev()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PollInterval != time.Second || cfg.MetricsAddr != ":9091" {
		t.Fatalf("defaults: %+v", cfg)
	}
	for _, bad := range []string{"", "http://mq", "amqp://"} {
		if _, err := LoadWorker(env(with(validDev(), "RABBITMQ_URL", bad))); err == nil {
			t.Errorf("RABBITMQ_URL %q accepted", bad)
		}
	}
	if _, err := LoadWorker(env(with(validProd(), "RABBITMQ_URL", "amqp://u:p@mq:5672/"))); err == nil {
		t.Error("plaintext amqp accepted in production")
	}
	if _, err := LoadWorker(env(validProd())); err != nil {
		t.Errorf("valid production worker: %v", err)
	}
}

func TestLoadAPIReportsAllErrors(t *testing.T) {
	t.Parallel()
	_, err := LoadAPI(env(map[string]string{"LOG_LEVEL": "loud", "HTTP_READ_TIMEOUT": "-1s"}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"APP_ENV", "DATABASE_URL", "LOG_LEVEL", "HTTP_READ_TIMEOUT", "API_KEY_PEPPER", "PUBLIC_BASE_URL", "CALLBACK_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadAPIRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]string{
		"unknown env":            with(validDev(), "APP_ENV", "prod"),
		"short pepper":           with(validDev(), "API_KEY_PEPPER", "short"),
		"short token":            with(validDev(), "CALLBACK_TOKEN", "abc"),
		"token charset":          with(validDev(), "CALLBACK_TOKEN", strings.Repeat("t", 31)+"/"),
		"relative base url":      with(validDev(), "PUBLIC_BASE_URL", "/callbacks"),
		"http base in prod":      with(validProd(), "PUBLIC_BASE_URL", "http://api.example.com"),
		"debug in prod":          with(validProd(), "LOG_LEVEL", "debug"),
		"testprovider in prod":   with(validProd(), "TESTPROVIDER_ENABLED", "true", "TESTPROVIDER_CALLBACK_SECRET", strings.Repeat("s", 16)),
		"testprovider no secret": with(validDev(), "TESTPROVIDER_ENABLED", "true"),
		"bad bool":               with(validDev(), "TESTPROVIDER_ENABLED", "yes"),
		"write <= provider":      with(validDev(), "HTTP_WRITE_TIMEOUT", "30s", "PROVIDER_TIMEOUT", "30s"),
		"no webhook key":         with(validDev(), "WEBHOOK_SECRETS_KEY", ""),
		"short webhook key":      with(validDev(), "WEBHOOK_SECRETS_KEY", "c2hvcnQ="),
		"webhook key not b64":    with(validDev(), "WEBHOOK_SECRETS_KEY", "!!!"),
		"private targets prod":   with(validProd(), "WEBHOOK_ALLOW_PRIVATE_TARGETS", "true"),
	}
	for name, m := range cases {
		if _, err := LoadAPI(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadAPI(env(validProd())); err != nil {
		t.Errorf("valid production config rejected: %v", err)
	}
	cfg, err := LoadAPI(env(with(validDev(), "TESTPROVIDER_ENABLED", "true", "TESTPROVIDER_CALLBACK_SECRET", strings.Repeat("s", 16))))
	if err != nil || !cfg.TestProviderEnabled {
		t.Errorf("testprovider in development: %v", err)
	}
}

func TestDeployedEnvironmentsRequireDatabaseTLS(t *testing.T) {
	t.Parallel()
	for _, e := range []string{"sandbox", "production"} {
		for _, tt := range []struct {
			query string
			ok    bool
		}{
			{"", false}, {"?sslmode=disable", false}, {"?sslmode=prefer", false},
			{"?sslmode=require", true}, {"?sslmode=verify-full", true},
		} {
			m := with(validProd(), "APP_ENV", e, "DATABASE_URL", "postgres://u:p@db.internal:5432/axene"+tt.query)
			if _, err := LoadAPI(env(m)); (err == nil) != tt.ok {
				t.Errorf("%s %q: err = %v, want ok=%v", e, tt.query, err, tt.ok)
			}
		}
	}
}

func TestInvalidDatabaseURLDoesNotLeakCredentials(t *testing.T) {
	t.Parallel()
	_, err := LoadAPI(env(with(validDev(), "DATABASE_URL", "mysql://admin:hunter2@db/axene")))
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
		fmt.Sprintf("%+v", API{APIKeyPepper: s, Common: Common{DatabaseURL: s, CallbackToken: s}}),
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
