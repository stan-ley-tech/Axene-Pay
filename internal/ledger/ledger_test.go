package ledger

import (
	"errors"
	"math"
	"testing"

	"github.com/stan-ley-tech/axene-pay/internal/platform/money"
)

func kes(minor int64) money.Amount {
	a, _ := money.New(minor, money.MustCurrency("KES"))
	return a
}

func entry(lines ...Line) Entry {
	return Entry{Book: BookAxene, Reason: ReasonFeeAccrued, SourceType: "fee", SourceID: "fee_1", Description: "test", Lines: lines}
}

var (
	receivable = Account{Book: BookAxene, Code: "fees_receivable:mer_1", Type: Asset}
	revenue    = Account{Book: BookAxene, Code: "fee_revenue", Type: Revenue}
)

func TestValidate(t *testing.T) {
	t.Parallel()
	ok := entry(Line{receivable, Debit, kes(150)}, Line{revenue, Credit, kes(150)})
	if err := ok.Validate(); err != nil {
		t.Fatalf("balanced entry rejected: %v", err)
	}
	usd, _ := money.New(150, money.MustCurrency("USD"))
	cases := map[string]struct {
		e    Entry
		want error
	}{
		"unbalanced":      {entry(Line{receivable, Debit, kes(150)}, Line{revenue, Credit, kes(149)}), ErrUnbalanced},
		"mixed currency":  {entry(Line{receivable, Debit, kes(150)}, Line{revenue, Credit, usd}), ErrUnbalanced},
		"one line":        {entry(Line{receivable, Debit, kes(150)}), ErrInvalid},
		"zero amount":     {entry(Line{receivable, Debit, kes(0)}, Line{revenue, Credit, kes(0)}), ErrInvalid},
		"negative amount": {entry(Line{receivable, Debit, kes(-5)}, Line{revenue, Credit, kes(-5)}), ErrInvalid},
		"bad direction":   {entry(Line{receivable, "sideways", kes(1)}, Line{revenue, Credit, kes(1)}), ErrInvalid},
		"wrong book": {entry(Line{Account{Book: BookSettlementMemo, Code: "x", Type: Memo}, Debit, kes(1)},
			Line{revenue, Credit, kes(1)}), ErrInvalid},
		"memo type in axene book": {entry(Line{Account{Book: BookAxene, Code: "x", Type: Memo}, Debit, kes(1)},
			Line{revenue, Credit, kes(1)}), ErrInvalid},
		"overflow": {entry(Line{receivable, Debit, kes(math.MaxInt64)}, Line{receivable, Debit, kes(math.MaxInt64)},
			Line{revenue, Credit, kes(1)}), ErrInvalid},
		"missing source": {Entry{Book: BookAxene, Reason: ReasonFeeAccrued, Description: "x",
			Lines: []Line{{receivable, Debit, kes(1)}, {revenue, Credit, kes(1)}}}, ErrInvalid},
	}
	for name, c := range cases {
		if err := c.e.Validate(); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	// Multi-currency entries balance per currency.
	usdRecv := Account{Book: BookAxene, Code: "fees_receivable:mer_1", Type: Asset}
	multi := entry(Line{receivable, Debit, kes(100)}, Line{revenue, Credit, kes(100)},
		Line{usdRecv, Debit, usd}, Line{revenue, Credit, usd})
	if err := multi.Validate(); err != nil {
		t.Fatalf("per-currency balanced entry rejected: %v", err)
	}
}
