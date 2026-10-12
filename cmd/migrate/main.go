// Command migrate applies or reports PostgreSQL schema migrations.
//
// Usage:
//
//	migrate up       apply all pending migrations
//	migrate status   list migrations and whether each is applied
//
// The connection string is read from DATABASE_URL. Migrations run as a
// separate deployment step, never automatically on API start (ADR-0006).
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/stan-ley-tech/axene-pay/internal/platform/db"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: migrate up|status")
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "up":
		applied, err := db.Migrate(ctx, url)
		for _, a := range applied {
			fmt.Println("applied", a)
		}
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("no pending migrations")
		}
		return nil
	case "status":
		st, err := db.Status(ctx, url)
		if err != nil {
			return err
		}
		for _, s := range st {
			state := "pending"
			if s.Applied {
				state = "applied"
			}
			fmt.Printf("%05d  %-8s %s\n", s.Version, state, s.Path)
		}
		return nil
	default:
		return fmt.Errorf("unknown command %q (want up or status)", args[0])
	}
}
