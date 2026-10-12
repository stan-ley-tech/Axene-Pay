// Command worker runs Axene's background work: finishing interrupted
// submissions, verifying provider outcomes by status query, relaying the
// transactional outbox to RabbitMQ, fanning out and delivering merchant
// webhooks, and pruning expired idempotency keys.
//
// Every task is safe to run in several worker instances at once: work is
// claimed in PostgreSQL with leases and SKIP LOCKED.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stan-ley-tech/axene-pay/internal/outbox"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/config"
	"github.com/stan-ley-tech/axene-pay/internal/platform/db"
	"github.com/stan-ley-tech/axene-pay/internal/platform/observability"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/providers/testprovider"
	"github.com/stan-ley-tech/axene-pay/internal/webhooks"
)

const batch = 50

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWorker(os.Getenv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", "worker", "env", string(cfg.Env))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer pool.Close()

	reg := providers.NewRegistry(cfg.Env == config.EnvProduction)
	if cfg.TestProviderEnabled {
		if err := reg.Register(testprovider.New(cfg.TestProviderCallbackSecret.Reveal())); err != nil {
			return err
		}
	}
	policy := payments.DefaultPolicy()
	policy.ProviderTimeout = cfg.ProviderTimeout
	metrics := observability.New()
	observability.Serve(ctx, cfg.MetricsAddr, metrics, logger)
	svc, err := payments.NewService(payments.Deps{
		Pool: pool, Providers: reg, CallbackURL: cfg.CallbackURL, Policy: policy, Logger: logger, Metrics: metrics,
	})
	if err != nil {
		return err
	}

	sealer, err := webhooks.NewSealer([]byte(cfg.WebhookSecretsKey.Reveal()), 1)
	if err != nil {
		return err
	}
	webhookSvc, err := webhooks.NewService(webhooks.Deps{
		Pool: pool, Sealer: sealer, Payments: svc, Policy: webhooks.DefaultPolicy(),
		RequireHTTPS: cfg.Env.IsDeployed(), AllowPrivateTargets: cfg.WebhookAllowPrivateTargets, Logger: logger,
	})
	if err != nil {
		return err
	}
	go webhooks.RunConsumer(ctx, cfg.RabbitMQURL.Reveal(), webhookSvc, logger)

	pub, err := outbox.NewRabbitPublisher(cfg.RabbitMQURL.Reveal())
	if err != nil {
		return err
	}
	defer pub.Close()
	relay := outbox.NewRelay(pool, pub, logger, batch)

	logger.Info("worker started", "poll_interval", cfg.PollInterval, "providers", reg.Names())
	w := worker{svc: svc, webhooks: webhookSvc, relay: relay, metrics: metrics, log: logger}
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		w.tick(ctx)
		if time.Since(lastPrune) > time.Minute {
			if n, err := svc.PruneIdempotencyKeys(ctx, 1000); err != nil {
				logger.Error("prune idempotency keys", "err", err)
			} else if n > 0 {
				logger.Info("pruned idempotency keys", "count", n)
			}
			lastPrune = time.Now()
		}
		select {
		case <-ctx.Done():
			logger.Info("worker stopping")
			return nil
		case <-ticker.C:
		}
	}
}

type worker struct {
	svc      *payments.Service
	webhooks *webhooks.Service
	relay    *outbox.Relay
	metrics  *observability.Metrics
	log      *slog.Logger
}

// tick runs one round of every task. Each task drains up to a bounded
// number of batches so one backlog cannot starve the others.
func (w worker) tick(ctx context.Context) {
	if err := w.svc.SweepSubmissions(ctx, batch); err != nil {
		w.log.Error("sweep submissions", "err", err)
	}
	for i := 0; i < 10; i++ {
		n, err := w.svc.VerifyDue(ctx, batch)
		if err != nil {
			w.log.Error("verify", "err", err)
			break
		}
		if n < batch {
			break
		}
	}
	for i := 0; i < 10; i++ {
		n, err := w.relay.RunOnce(ctx)
		if err != nil {
			w.log.Error("outbox relay", "err", err)
			break
		}
		if n < batch {
			break
		}
	}
	if _, err := w.webhooks.SweepUnfanned(ctx, batch); err != nil {
		w.log.Error("webhook sweep", "err", err)
	}
	for i := 0; i < 10; i++ {
		n, err := w.webhooks.DeliverDue(ctx, batch)
		if err != nil {
			w.log.Error("webhook delivery", "err", err)
			break
		}
		if n < batch {
			break
		}
	}
	if n, age, err := w.relay.Backlog(ctx); err == nil {
		w.metrics.SetOutboxBacklog(n, age)
	} else {
		w.log.Error("outbox backlog", "err", err)
	}
}
