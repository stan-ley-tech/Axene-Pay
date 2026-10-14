// Command api runs the Axene Payments HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/stan-ley-tech/axene-pay/internal/accounting"
	"github.com/stan-ley-tech/axene-pay/internal/fees"
	"github.com/stan-ley-tech/axene-pay/internal/httpapi"
	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/config"
	"github.com/stan-ley-tech/axene-pay/internal/platform/db"
	"github.com/stan-ley-tech/axene-pay/internal/platform/observability"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/providers/testprovider"
	"github.com/stan-ley-tech/axene-pay/internal/webhooks"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet (config failure), so write plainly.
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadAPI(os.Getenv)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", "api", "env", string(cfg.Env))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The pool connects lazily, so the API starts even if the database is
	// briefly unavailable; /readyz reports the real state.
	pool, err := db.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer pool.Close()

	registry, err := Registry(cfg.Common)
	if err != nil {
		return err
	}
	merchants, err := merchant.NewService(pool, []byte(cfg.APIKeyPepper.Reveal()))
	if err != nil {
		return err
	}
	policy := payments.DefaultPolicy()
	policy.ProviderTimeout = cfg.ProviderTimeout
	metrics := observability.New()
	observability.Serve(ctx, cfg.MetricsAddr, metrics, logger)
	feeSvc, err := fees.NewService(fees.Deps{Pool: pool, Production: cfg.Env == config.EnvProduction, Policy: fees.DefaultPolicy(), Logger: logger})
	if err != nil {
		return err
	}
	paymentSvc, err := payments.NewService(payments.Deps{
		Pool: pool, Providers: registry, CallbackURL: cfg.CallbackURL, Policy: policy, Logger: logger, Metrics: metrics,
		Effects: &accounting.Effects{Fees: feeSvc},
	})
	if err != nil {
		return err
	}
	sealer, err := webhooks.NewSealer([]byte(cfg.WebhookSecretsKey.Reveal()), 1)
	if err != nil {
		return err
	}
	webhookSvc, err := webhooks.NewService(webhooks.Deps{
		Pool: pool, Sealer: sealer, Payments: paymentSvc, Policy: webhooks.DefaultPolicy(),
		RequireHTTPS: cfg.Env.IsDeployed(), AllowPrivateTargets: cfg.WebhookAllowPrivateTargets, Logger: logger,
	})
	if err != nil {
		return err
	}
	logger.Info("providers registered", "providers", registry.Names())

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Logger: logger, DB: pool, Auth: merchants, Payments: paymentSvc, CallbackToken: cfg.CallbackToken.Reveal(),
			Webhooks: webhookSvc, HTTPMetrics: metrics.HTTPMiddleware,
		}),
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       2 * cfg.WriteTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "timeout", cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}

// Registry builds the provider registry for this deployment. Real provider
// adapters are added here as they are implemented (M-Pesa: Phase 2).
func Registry(c config.Common) (*providers.Registry, error) {
	reg := providers.NewRegistry(c.Env == config.EnvProduction)
	if c.TestProviderEnabled {
		if err := reg.Register(testprovider.New(c.TestProviderCallbackSecret.Reveal())); err != nil {
			return nil, err
		}
	}
	return reg, nil
}
