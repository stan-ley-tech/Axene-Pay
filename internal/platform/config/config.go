// Package config loads and validates process configuration from the
// environment. Configuration is read once at startup; an invalid
// configuration is a fatal error that reports every problem at once.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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

// API is the configuration of the api process.
type API struct {
	Env             Environment
	HTTPAddr        string
	LogLevel        slog.Level
	DatabaseURL     Secret
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

// LoadAPI builds API configuration using getenv (normally os.Getenv).
func LoadAPI(getenv func(string) string) (API, error) {
	p := parser{getenv: getenv}
	cfg := API{
		Env:             p.environment("APP_ENV"),
		HTTPAddr:        p.stringOr("HTTP_ADDR", ":8080"),
		LogLevel:        p.logLevel("LOG_LEVEL", slog.LevelInfo),
		DatabaseURL:     Secret(p.required("DATABASE_URL")),
		ReadTimeout:     p.duration("HTTP_READ_TIMEOUT", 10*time.Second),
		WriteTimeout:    p.duration("HTTP_WRITE_TIMEOUT", 30*time.Second),
		ShutdownTimeout: p.duration("HTTP_SHUTDOWN_TIMEOUT", 20*time.Second),
	}
	if raw := cfg.DatabaseURL.Reveal(); raw != "" {
		p.check(validateDatabaseURL(raw, cfg.Env))
	}
	if cfg.Env.IsDeployed() && cfg.LogLevel < slog.LevelInfo {
		p.fail("LOG_LEVEL: debug logging is not allowed in %s", cfg.Env)
	}
	if err := p.err(); err != nil {
		return API{}, err
	}
	return cfg, nil
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
