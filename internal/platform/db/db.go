// Package db opens PostgreSQL connections and applies schema migrations.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx", used only by migrations
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/stan-ley-tech/axene-pay/migrations"
)

// Open returns a connection pool. Connections are established lazily, so a
// briefly unavailable database does not prevent startup.
func Open(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// pgx error text can include the URL; do not propagate it.
		return nil, errors.New("db: invalid database URL")
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 10 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Migrate applies all pending migrations. A PostgreSQL advisory lock ensures
// concurrent runs (e.g. two deploy jobs) apply each migration once.
func Migrate(ctx context.Context, url string) ([]string, error) {
	p, closeFn, err := provider(url)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	results, err := p.Up(ctx)
	applied := make([]string, 0, len(results))
	for _, r := range results {
		applied = append(applied, r.Source.Path)
	}
	if err != nil {
		return applied, fmt.Errorf("db: migrate up: %w", err)
	}
	return applied, nil
}

// MigrateDownAll rolls back every migration. It exists for tests that verify
// Down sections; production migrations are forward-only (ADR-0006).
func MigrateDownAll(ctx context.Context, url string) error {
	p, closeFn, err := provider(url)
	if err != nil {
		return err
	}
	defer closeFn()
	if _, err := p.DownTo(ctx, 0); err != nil {
		return fmt.Errorf("db: migrate down: %w", err)
	}
	return nil
}

// MigrationStatus describes one migration and whether it has been applied.
type MigrationStatus struct {
	Version int64
	Path    string
	Applied bool
}

// Status reports the state of every known migration.
func Status(ctx context.Context, url string) ([]MigrationStatus, error) {
	p, closeFn, err := provider(url)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	st, err := p.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: migration status: %w", err)
	}
	out := make([]MigrationStatus, 0, len(st))
	for _, s := range st {
		out = append(out, MigrationStatus{Version: s.Source.Version, Path: s.Source.Path, Applied: s.State == goose.StateApplied})
	}
	return out, nil
}

func provider(url string) (*goose.Provider, func(), error) {
	if _, err := pgx.ParseConfig(url); err != nil {
		return nil, nil, errors.New("db: invalid database URL")
	}
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return nil, nil, errors.New("db: invalid database URL")
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("db: session locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("db: migration provider: %w", err)
	}
	return p, func() { _ = sqlDB.Close() }, nil
}
