// Package reconciliation imports provider settlement statements and matches
// them against Axene's payments, fees, and ledger (docs/ledger.md §6). It
// never edits records to make reports agree: discrepancies become
// exceptions that an identified operator resolves.
package reconciliation

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
)

// StatementHeader is the header of Axene's normalized settlement statement
// format, version 1. Provider-specific statements are converted to this
// format by provider adapters once their real formats are known; this is
// Axene's format, not any provider's.
var StatementHeader = []string{
	"provider_reference", "receipt", "destination", "currency", "gross", "provider_fee", "net", "settled_at", "axene_fee_collected",
}

// StatementLine is one parsed line. Amounts are integer minor units.
type StatementLine struct {
	Number            int
	ProviderReference string
	Receipt           string
	Destination       string
	Currency          money.Currency
	Gross             int64
	ProviderFee       int64
	Net               int64
	AxeneFeeCollected *int64
	SettledAt         time.Time
}

// ErrStatement wraps statement format errors.
var ErrStatement = errors.New("reconciliation: invalid statement")

// MaxStatementLines bounds a single import.
const MaxStatementLines = 100_000

// ParseStatement strictly parses a normalized statement. Any malformed line
// rejects the whole file: a partial import would leave settlement data
// silently incomplete.
func ParseStatement(r io.Reader) ([]StatementLine, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = len(StatementHeader)
	cr.TrimLeadingSpace = false
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("%w: cannot read header: %v", ErrStatement, err)
	}
	for i, h := range StatementHeader {
		if header[i] != h {
			return nil, fmt.Errorf("%w: header column %d is %q, want %q", ErrStatement, i+1, header[i], h)
		}
	}
	var lines []StatementLine
	for n := 2; ; n++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %v", ErrStatement, n, err)
		}
		if len(lines) >= MaxStatementLines {
			return nil, fmt.Errorf("%w: more than %d lines", ErrStatement, MaxStatementLines)
		}
		l, err := parseLine(n, rec)
		if err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: no lines", ErrStatement)
	}
	return lines, nil
}

func parseLine(n int, rec []string) (StatementLine, error) {
	bad := func(field, why string) error {
		return fmt.Errorf("%w: line %d, %s: %s", ErrStatement, n, field, why)
	}
	l := StatementLine{Number: n, ProviderReference: strings.TrimSpace(rec[0]), Receipt: strings.TrimSpace(rec[1]),
		Destination: strings.TrimSpace(rec[2])}
	if l.ProviderReference == "" && l.Receipt == "" {
		return l, bad("provider_reference/receipt", "at least one is required")
	}
	if l.Destination == "" {
		return l, bad("destination", "required")
	}
	cur, err := money.ParseCurrency(rec[3])
	if err != nil {
		return l, bad("currency", err.Error())
	}
	l.Currency = cur
	ints := []*int64{&l.Gross, &l.ProviderFee, &l.Net}
	for i, field := range []string{"gross", "provider_fee", "net"} {
		v, err := strconv.ParseInt(rec[4+i], 10, 64)
		if err != nil || v < 0 {
			return l, bad(field, "must be a non-negative integer in minor units")
		}
		*ints[i] = v
	}
	if l.Gross == 0 {
		return l, bad("gross", "must be positive")
	}
	if rec[8] != "" {
		v, err := strconv.ParseInt(rec[8], 10, 64)
		if err != nil || v < 0 {
			return l, bad("axene_fee_collected", "must be empty or a non-negative integer")
		}
		l.AxeneFeeCollected = &v
	}
	collected := int64(0)
	if l.AxeneFeeCollected != nil {
		collected = *l.AxeneFeeCollected
	}
	if l.ProviderFee+l.Net+collected != l.Gross {
		return l, bad("amounts", "gross must equal provider_fee + net + axene_fee_collected")
	}
	t, err := time.Parse(time.RFC3339, rec[7])
	if err != nil {
		return l, bad("settled_at", "must be RFC 3339")
	}
	l.SettledAt = t.UTC()
	return l, nil
}
