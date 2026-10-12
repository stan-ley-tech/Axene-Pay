// Package config loads and validates process configuration from the
// environment. Configuration is read once at startup; an invalid
// configuration is a fatal error that reports every problem at once.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Environment identifies the deployment environment.
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvTest        Environment = "test"
	EnvSandbox     Environment = "sandbox"
	EnvProduction  Environment = "production"
)

// IsDeployed reports whether the environment is a shared, non-local
// deployment where insecure development defaults are forbidden.
func (e Environment) IsDeployed() bool {
	return e == EnvSandbox || e == EnvProduction
}

// Common is configuration shared by the api and worker processes.
type Common struct {
	Env         Environment
	LogLevel    slog.Level
	DatabaseURL Secret
	MetricsAddr string

	// PublicBaseURL is the externally reachable base URL providers call back.
	PublicBaseURL string
	// CallbackToken is a high-entropy path segment in provider callback URLs
	// (ADR-0009: callbacks are untrusted; the token raises the bar for forgery).
	CallbackToken Secret

	ProviderTimeout time.Duration

	// TestProviderEnabled registers the simulated provider. Forbidden in
	// production.
	TestProviderEnabled        bool
	TestProviderCallbackSecret Secret
}

// API is the configuration of the api process.
type API struct {
	Common
	HTTPAddr        string
	APIKeyPepper    Secret
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// Worker is the configuration of the worker process.
type Worker struct {
	Common
	RabbitMQURL  Secret
	PollInterval time.Duration
}

// LoadAPI builds API configuration using getenv (normally os.Getenv).
func LoadAPI(getenv func(string) string) (API, error) {
	p := parser{getenv: getenv}
	cfg := API{
		Common:          loadCommon(&p, ":9090"),
		HTTPAddr:        p.stringOr("HTTP_ADDR", ":8080"),
		APIKeyPepper:    Secret(p.required("API_KEY_PEPPER")),
		ReadTimeout:     p.duration("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    p.duration("HTTP_WRITE_TIMEOUT", 60*time.Second),
		ShutdownTimeout: p.duration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
	}
	if v := cfg.APIKeyPepper.Reveal(); v != "" && len(v) < 32 {
		p.fail("API_KEY_PEPPER: must be at least 32 characters")
	}
	if cfg.WriteTimeout <= cfg.ProviderTimeout {
		p.fail("HTTP_WRITE_TIMEOUT: must exceed PROVIDER_TIMEOUT so payment creation can finish")
	}
	if err := p.err(); err != nil {
		return API{}, err
	}
	return cfg, nil
}

// LoadWorker builds worker configuration using getenv.
func LoadWorker(getenv func(string) string) (Worker, error) {
	p := parser{getenv: getenv}
	cfg := Worker{
		Common:       loadCommon(&p, ":9091"),
		RabbitMQURL:  Secret(p.required("RABBITMQ_URL")),
		PollInterval: p.duration("WORKER_POLL_INTERVAL", time.Second),
	}
	if raw := cfg.RabbitMQURL.Reveal(); raw != "" {
		u, err := url.Parse(raw)
		switch {
		case err != nil || (u.Scheme != "amqp" && u.Scheme != "amqps") || u.Host == "":
			p.fail("RABBITMQ_URL: must be an amqp:// or amqps:// URL with a host")
		case cfg.Env.IsDeployed() && u.Scheme != "amqps":
			p.fail("RABBITMQ_URL: must use amqps in %s", cfg.Env)
		}
	}
	if err := p.err(); err != nil {
		return Worker{}, err
	}
	return cfg, nil
}

var tokenChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func loadCommon(p *parser, defaultMetrics string) Common {
	c := Common{
		Env:                        p.environment("APP_ENV"),
		LogLevel:                   p.logLevel("LOG_LEVEL", slog.LevelInfo),
		DatabaseURL:                Secret(p.required("DATABASE_URL")),
		MetricsAddr:                p.stringOr("METRICS_ADDR", defaultMetrics),
		PublicBaseURL:              strings.TrimRight(p.required("PUBLIC_BASE_URL"), "/"),
		CallbackToken:              Secret(p.required("CALLBACK_TOKEN")),
		ProviderTimeout:            p.duration("PROVIDER_TIMEOUT", 30*time.Second),
		TestProviderEnabled:        p.boolean("TESTPROVIDER_ENABLED"),
		TestProviderCallbackSecret: Secret(p.lookup("TESTPROVIDER_CALLBACK_SECRET")),
	}
	if raw := c.DatabaseURL.Reveal(); raw != "" {
		p.check(validateDatabaseURL(raw, c.Env))
	}
	if c.Env.IsDeployed() && c.LogLevel < slog.LevelInfo {
		p.fail("LOG_LEVEL: debug logging is not allowed in %s", c.Env)
	}
	if c.PublicBaseURL != "" {
		u, err := url.Parse(c.PublicBaseURL)
		switch {
		case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			p.fail("PUBLIC_BASE_URL: must be an absolute http(s) URL")
		case c.Env.IsDeployed() && u.Scheme != "https":
			p.fail("PUBLIC_BASE_URL: must use https in %s", c.Env)
		}
	}
	if t := c.CallbackToken.Reveal(); t != "" && (len(t) < 32 || !tokenChars.MatchString(t)) {
		p.fail("CALLBACK_TOKEN: must be at least 32 characters of A-Z a-z 0-9 _ -")
	}
	if c.TestProviderEnabled {
		if c.Env == EnvProduction {
			p.fail("TESTPROVIDER_ENABLED: the simulated provider is not allowed in production")
		}
		if len(c.TestProviderCallbackSecret.Reveal()) < 16 {
			p.fail("TESTPROVIDER_CALLBACK_SECRET: required (at least 16 characters) when TESTPROVIDER_ENABLED=true")
		}
	}
	return c
}

// CallbackURL is the URL a provider is told to call back.
func (c Common) CallbackURL(provider string) string {
	return c.PublicBaseURL + "/callbacks/" + provider + "/" + c.CallbackToken.Reveal()
}

// validateDatabaseURL requires a postgres URL and, outside local
// environments, TLS to the database.
func validateDatabaseURL(raw string, env Environment) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		// Do not echo the value: it contains credentials.
		return errors.New("DATABASE_URL: must be a postgres:// URL with a host")
	}
	if env.IsDeployed() {
		switch u.Query().Get("sslmode") {
		case "require", "verify-ca", "verify-full":
		default:
			return fmt.Errorf("DATABASE_URL: sslmode must be require, verify-ca or verify-full in %s", env)
		}
	}
	return nil
}

// Secret holds a sensitive string. It never prints its value through fmt
// or slog; call Reveal at the single point of use.
type Secret string

func (s Secret) Reveal() string             { return string(s) }
func (Secret) String() string               { return "[REDACTED]" }
func (Secret) GoString() string             { return "[REDACTED]" }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[REDACTED]") }
func (Secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// parser accumulates validation errors so all problems are reported together.
type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *parser) check(err error) {
	if err != nil {
		p.errs = append(p.errs, err)
	}
}

func (p *parser) err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration: %w", errors.Join(p.errs...))
}

func (p *parser) lookup(key string) string { return strings.TrimSpace(p.getenv(key)) }

func (p *parser) required(key string) string {
	v := p.lookup(key)
	if v == "" {
		p.fail("%s: required", key)
	}
	return v
}

func (p *parser) stringOr(key, def string) string {
	if v := p.lookup(key); v != "" {
		return v
	}
	return def
}

func (p *parser) boolean(key string) bool {
	switch v := p.lookup(key); v {
	case "", "false":
		return false
	case "true":
		return true
	default:
		p.fail("%s: must be true or false, got %q", key, v)
		return false
	}
}

func (p *parser) environment(key string) Environment {
	v := Environment(p.lookup(key))
	switch v {
	case EnvDevelopment, EnvTest, EnvSandbox, EnvProduction:
		return v
	case "":
		// No default: running production code with a guessed environment is unsafe.
		p.fail("%s: required (development, test, sandbox, production)", key)
	default:
		p.fail("%s: unknown environment %q", key, string(v))
	}
	return ""
}

func (p *parser) logLevel(key string, def slog.Level) slog.Level {
	v := p.lookup(key)
	if v == "" {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail("%s: invalid log level %q", key, v)
		return def
	}
	return l
}

func (p *parser) duration(key string, def time.Duration) time.Duration {
	v := p.lookup(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		p.fail("%s: must be a positive duration like 10s, got %q", key, v)
		return def
	}
	return d
}
