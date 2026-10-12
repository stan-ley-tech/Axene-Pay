// Package testdb gives integration tests an isolated, fully migrated
// PostgreSQL database.
//
// Tests using it are built with the "integration" tag and need
// TEST_DATABASE_URL pointing at a server where the role may CREATE DATABASE.
// Each call creates a fresh database and drops it when the test ends, so
// tests never share state and can run in parallel.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/platform/db"
)

// URL returns TEST_DATABASE_URL or fails the test when it is not set.
// Integration tests must not silently skip: a missing database is a setup
// error that should be visible.
func URL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Fatal("TEST_DATABASE_URL is not set (integration tests need PostgreSQL)")
	}
	return u
}

// NewURL creates an empty database and returns its connection URL. The
// database is dropped when the test finishes.
func NewURL(t testing.TB) string {
	t.Helper()
	base := URL(t)
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "axene_t_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close(context.Background())
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// New returns a pool connected to a fresh, fully migrated database.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	u := NewURL(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, u); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	pool, err := db.Open(ctx, u)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
