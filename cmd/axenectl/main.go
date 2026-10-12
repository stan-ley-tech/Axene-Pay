// Command axenectl performs administrative setup of merchants until the
// operations portal exists (Phase 4).
//
// Reads DATABASE_URL and API_KEY_PEPPER from the environment.
//
//	axenectl merchant create --name "Duka Ltd"
//	axenectl merchant set-status --merchant mer_... --status approved
//	axenectl destination create --merchant mer_... --env sandbox --type mpesa_paybill --identifier 600123 [--label "Main"]
//	axenectl provider-config create --merchant mer_... --env sandbox --provider testprovider --destination dst_... [--priority 100]
//	axenectl apikey create --merchant mer_... --env sandbox
//	axenectl apikey revoke --key key_...
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/platform/db"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "axenectl:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  axenectl merchant create --name NAME
  axenectl merchant set-status --merchant ID --status pending_review|approved|suspended
  axenectl destination create --merchant ID --env sandbox|live --type mpesa_paybill|mpesa_till --identifier DIGITS [--label TEXT]
  axenectl provider-config create --merchant ID --env sandbox|live --provider NAME --destination ID [--method mpesa] [--priority N]
  axenectl apikey create --merchant ID --env sandbox|live
  axenectl apikey revoke --key ID`

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 2 {
		return errors.New(usage)
	}
	cmd, flags := args[0]+" "+args[1], args[2:]

	dbURL, pepper := os.Getenv("DATABASE_URL"), os.Getenv("API_KEY_PEPPER")
	if dbURL == "" || pepper == "" {
		return errors.New("DATABASE_URL and API_KEY_PEPPER are required")
	}
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	svc, err := merchant.NewService(pool, []byte(pepper))
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	merchantID := fs.String("merchant", "", "merchant ID")
	envName := fs.String("env", "", "sandbox or live")
	name := fs.String("name", "", "merchant name")
	status := fs.String("status", "", "merchant status")
	typ := fs.String("type", "", "destination type")
	identifier := fs.String("identifier", "", "destination identifier")
	label := fs.String("label", "", "destination label")
	provider := fs.String("provider", "", "provider name")
	method := fs.String("method", "mpesa", "payment method")
	destination := fs.String("destination", "", "settlement destination ID")
	priority := fs.Int("priority", 100, "routing priority (lower first)")
	keyID := fs.String("key", "", "API key ID")
	if err := fs.Parse(flags); err != nil {
		return fmt.Errorf("%v\n%s", err, usage)
	}
	env := func() (merchant.Environment, error) { return merchant.ParseEnvironment(*envName) }

	switch cmd {
	case "merchant create":
		m, err := svc.CreateMerchant(ctx, *name)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "merchant %s created (status %s)\n", m.PublicID, m.Status)
	case "merchant set-status":
		m, err := svc.SetStatus(ctx, *merchantID, merchant.Status(*status))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "merchant %s status %s\n", m.PublicID, m.Status)
	case "destination create":
		e, err := env()
		if err != nil {
			return err
		}
		d, err := svc.CreateDestination(ctx, *merchantID, e, *typ, *identifier, *label)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "destination %s created (%s %s, %s)\n", d.PublicID, d.Type, d.Identifier, d.Environment)
	case "provider-config create":
		e, err := env()
		if err != nil {
			return err
		}
		c, err := svc.CreateProviderConfig(ctx, *merchantID, e, *method, *provider, *destination, int32(*priority))
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "provider config %s created (%s via %s, priority %d)\n", c.PublicID, c.Method, c.Provider, c.Priority)
	case "apikey create":
		e, err := env()
		if err != nil {
			return err
		}
		k, err := svc.IssueAPIKey(ctx, *merchantID, e)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "api key %s created for %s\n", k.ID, k.Environment)
		fmt.Fprintf(out, "secret (shown once, store it securely): %s\n", k.Secret)
	case "apikey revoke":
		if err := svc.RevokeAPIKey(ctx, *keyID); err != nil {
			return err
		}
		fmt.Fprintf(out, "api key %s revoked\n", *keyID)
	default:
		return errors.New(usage)
	}
	return nil
}
