package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stan-ley-tech/axene-pay/internal/accounting"
	"github.com/stan-ley-tech/axene-pay/internal/fees"
	"github.com/stan-ley-tech/axene-pay/internal/merchant"
	"github.com/stan-ley-tech/axene-pay/internal/payments"
	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
	"github.com/stan-ley-tech/axene-pay/internal/providers"
	"github.com/stan-ley-tech/axene-pay/internal/reconciliation"
	"github.com/stan-ley-tech/axene-pay/internal/store"
)

const financeUsage = `finance commands (every change requires --operator; all are audited):
  axenectl fee-schedule create --env sandbox|live --currency KES --percentage-ppm N --fixed N --rounding half_up|half_even|up|down
                               --fee-payer merchant --provider-fee-bearer merchant|axene --reversal-policy refund_fee|retain_fee|pending_approval
                               --effective-from RFC3339 [--merchant ID] [--provider NAME] [--minimum N] [--maximum N] [--description TEXT]
  axenectl fee-schedule activate --schedule ID --operator NAME --approval-ref REFERENCE
  axenectl fee-schedule end --schedule ID --at RFC3339 --operator NAME
  axenectl fee-terms set --merchant ID --env sandbox|live --mechanism none_configured --operator NAME
  axenectl fee waive --fee ID --operator NAME --reason TEXT
  axenectl settlement import --provider NAME --env sandbox|live --file PATH --source-ref TEXT --operator NAME
  axenectl recon run --provider NAME --env sandbox|live --operator NAME
  axenectl recon list [--open]
  axenectl recon resolve --exception ID --action acknowledge|payment_succeeded|payment_failed|payment_reversed|rematch --operator NAME --note TEXT

Amounts are integer minor units (KES 1.00 = 100). Percentages are parts per
million (1.5% = 15000). No rate is built in: schedules are operator data and
must be activated with an approval reference.`

func isFinance(cmd string) bool {
	for _, p := range []string{"fee-schedule ", "fee-terms ", "fee ", "settlement ", "recon "} {
		if strings.HasPrefix(cmd, p) {
			return true
		}
	}
	return false
}

func runFinance(ctx context.Context, cmd string, flags []string, pool *pgxpool.Pool, ms *merchant.Service, out io.Writer) error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	env := os.Getenv("APP_ENV")
	feeSvc, err := fees.NewService(fees.Deps{Pool: pool, Production: env == "production", Policy: fees.DefaultPolicy(), Logger: logger})
	if err != nil {
		return err
	}
	// Reconciliation applies outcomes through the payment state machine; it
	// never calls a provider, so no provider adapters are needed here.
	paySvc, err := payments.NewService(payments.Deps{
		Pool: pool, Providers: providers.NewRegistry(env == "production"), Policy: payments.DefaultPolicy(),
		CallbackURL: func(string) string { return "" }, Logger: logger, Effects: &accounting.Effects{Fees: feeSvc},
	})
	if err != nil {
		return err
	}
	recSvc, err := reconciliation.NewService(reconciliation.Deps{Pool: pool, Payments: paySvc, Fees: feeSvc,
		SettlementGrace: 72 * time.Hour, Logger: logger})
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := struct {
		env, currency, rounding, payer, bearer, policy, from, at, merchantID, provider, description *string
		ppm, fixed, minimum, maximum                                                                *int64
		schedule, operator, approvalRef, mechanism, fee, reason, file, sourceRef, exception, action *string
		note                                                                                        *string
		open                                                                                        *bool
	}{
		env: fs.String("env", "", ""), currency: fs.String("currency", "KES", ""), rounding: fs.String("rounding", "", ""),
		payer: fs.String("fee-payer", "", ""), bearer: fs.String("provider-fee-bearer", "", ""),
		policy: fs.String("reversal-policy", "", ""), from: fs.String("effective-from", "", ""), at: fs.String("at", "", ""),
		merchantID: fs.String("merchant", "", ""), provider: fs.String("provider", "", ""), description: fs.String("description", "", ""),
		ppm: fs.Int64("percentage-ppm", 0, ""), fixed: fs.Int64("fixed", 0, ""), minimum: fs.Int64("minimum", -1, ""),
		maximum: fs.Int64("maximum", -1, ""), schedule: fs.String("schedule", "", ""), operator: fs.String("operator", "", ""),
		approvalRef: fs.String("approval-ref", "", ""), mechanism: fs.String("mechanism", "", ""), fee: fs.String("fee", "", ""),
		reason: fs.String("reason", "", ""), file: fs.String("file", "", ""), sourceRef: fs.String("source-ref", "", ""),
		exception: fs.String("exception", "", ""), action: fs.String("action", "", ""), note: fs.String("note", "", ""),
		open: fs.Bool("open", false, ""),
	}
	if err := fs.Parse(flags); err != nil {
		return fmt.Errorf("%v\n%s", err, financeUsage)
	}
	requireOperator := func() error {
		if strings.TrimSpace(*f.operator) == "" {
			return errors.New("--operator is required")
		}
		return nil
	}
	merchantUUID := func() (pgtype.UUID, error) {
		if *f.merchantID == "" {
			return pgtype.UUID{}, nil
		}
		m, err := store.New(pool).GetMerchantByPublicID(ctx, *f.merchantID)
		if err != nil {
			return pgtype.UUID{}, fmt.Errorf("merchant %s: %w", *f.merchantID, err)
		}
		return m.ID, nil
	}

	switch cmd {
	case "fee-schedule create":
		from, err := time.Parse(time.RFC3339, *f.from)
		if err != nil {
			return errors.New("--effective-from must be RFC 3339")
		}
		mid, err := merchantUUID()
		if err != nil {
			return err
		}
		terms := fees.Terms{PercentagePPM: *f.ppm, Fixed: *f.fixed, Rounding: money.RoundingMode(*f.rounding)}
		if *f.minimum >= 0 {
			terms.Minimum = f.minimum
		}
		if *f.maximum >= 0 {
			terms.Maximum = f.maximum
		}
		sc, err := feeSvc.CreateSchedule(ctx, fees.ScheduleInput{MerchantID: mid, Environment: *f.env, Method: "mpesa", Provider: *f.provider,
			Currency: *f.currency, Terms: terms, FeePayer: *f.payer, ProviderFeeBearer: *f.bearer, ReversalPolicy: *f.policy,
			EffectiveFrom: from, Description: *f.description})
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "fee schedule %s created as draft (not pricing payments until activated with an approval reference)\n", sc.PublicID)
	case "fee-schedule activate":
		if err := requireOperator(); err != nil {
			return err
		}
		sc, err := feeSvc.Activate(ctx, *f.schedule, *f.operator, *f.approvalRef)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "fee schedule %s active from %s (approved by %s, ref %s)\n", sc.PublicID, sc.EffectiveFrom.Format(time.RFC3339), *sc.ApprovedBy, *sc.ApprovalReference)
	case "fee-schedule end":
		if err := requireOperator(); err != nil {
			return err
		}
		at, err := time.Parse(time.RFC3339, *f.at)
		if err != nil {
			return errors.New("--at must be RFC 3339")
		}
		sc, err := feeSvc.End(ctx, *f.schedule, at, *f.operator)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "fee schedule %s ends %s (status %s)\n", sc.PublicID, sc.EffectiveTo.Format(time.RFC3339), sc.Status)
	case "fee-terms set":
		if err := requireOperator(); err != nil {
			return err
		}
		mid, err := merchantUUID()
		if err != nil || !mid.Valid {
			return errors.New("--merchant is required")
		}
		if _, err := merchant.ParseEnvironment(*f.env); err != nil {
			return err
		}
		if err := feeSvc.SetMechanism(ctx, mid, *f.env, fees.Mechanism(*f.mechanism), *f.operator); err != nil {
			return err
		}
		fmt.Fprintf(out, "fee collection mechanism for %s (%s): %s\n", *f.merchantID, *f.env, *f.mechanism)
	case "fee waive":
		if err := requireOperator(); err != nil {
			return err
		}
		fee, err := feeSvc.Waive(ctx, *f.fee, *f.operator, *f.reason, "cli-waive-"+*f.fee)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "fee %s waived\n", fee.PublicID)
	case "settlement import":
		if err := requireOperator(); err != nil {
			return err
		}
		file, err := os.Open(*f.file)
		if err != nil {
			return err
		}
		defer file.Close()
		b, err := recSvc.Import(ctx, *f.provider, *f.env, *f.sourceRef, *f.operator, file)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "settlement batch %s imported (%d lines); run reconciliation to match it\n", b.PublicID, b.LineCount)
	case "recon run":
		if err := requireOperator(); err != nil {
			return err
		}
		res, err := recSvc.Run(ctx, *f.provider, *f.env, *f.operator)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "reconciliation run %s: %d lines matched, %d exceptions\n", res.RunID, res.Matched, res.Exceptions)
	case "recon list":
		xs, err := recSvc.Exceptions(ctx, *f.open, 500)
		if err != nil {
			return err
		}
		for _, x := range xs {
			fmt.Fprintf(out, "%s  %-26s %-8s %s  %s\n", x.PublicID, x.Type, x.Status, x.CreatedAt.Format(time.RFC3339), x.Detail)
		}
	case "recon resolve":
		if err := requireOperator(); err != nil {
			return err
		}
		x, err := recSvc.Resolve(ctx, *f.exception, reconciliation.Action(*f.action), *f.operator, *f.note)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "exception %s resolved: %s\n", x.PublicID, *x.Resolution)
	default:
		return errors.New(financeUsage)
	}
	return nil
}
