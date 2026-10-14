package reconciliation

import (
	"errors"
	"strings"
	"testing"
)

const header = "provider_reference,receipt,destination,currency,gross,provider_fee,net,settled_at,axene_fee_collected\n"

func TestParseStatement(t *testing.T) {
	t.Parallel()
	lines, err := ParseStatement(strings.NewReader(header +
		"tp_1,RCPT1,600123,KES,10000,50,9950,2026-10-13T10:00:00Z,\n" +
		",RCPT2,600123,KES,20000,0,19700,2026-10-13T11:00:00+03:00,300\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("lines %d", len(lines))
	}
	l := lines[0]
	if l.Number != 2 || l.ProviderReference != "tp_1" || l.Gross != 10000 || l.ProviderFee != 50 || l.Net != 9950 || l.AxeneFeeCollected != nil {
		t.Fatalf("line 1 %+v", l)
	}
	if l2 := lines[1]; l2.AxeneFeeCollected == nil || *l2.AxeneFeeCollected != 300 || l2.SettledAt.Hour() != 8 || l2.ProviderReference != "" {
		t.Fatalf("line 2 %+v", l2)
	}
}

func TestParseStatementRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":          "",
		"wrong header":   "ref,receipt,destination,currency,gross,provider_fee,net,settled_at,axene_fee_collected\n",
		"no lines":       header,
		"no reference":   header + ",,600123,KES,100,0,100,2026-10-13T10:00:00Z,\n",
		"no destination": header + "r,,,KES,100,0,100,2026-10-13T10:00:00Z,\n",
		"bad currency":   header + "r,,600123,kes,100,0,100,2026-10-13T10:00:00Z,\n",
		"float amount":   header + "r,,600123,KES,100.5,0,100.5,2026-10-13T10:00:00Z,\n",
		"negative fee":   header + "r,,600123,KES,100,-1,101,2026-10-13T10:00:00Z,\n",
		"zero gross":     header + "r,,600123,KES,0,0,0,2026-10-13T10:00:00Z,\n",
		"sum mismatch":   header + "r,,600123,KES,100,10,80,2026-10-13T10:00:00Z,\n",
		"sum with fee":   header + "r,,600123,KES,100,10,80,2026-10-13T10:00:00Z,5\n",
		"bad date":       header + "r,,600123,KES,100,0,100,13/10/2026,\n",
		"missing column": header + "r,,600123,KES,100,0,100,2026-10-13T10:00:00Z\n",
		"bad axene fee":  header + "r,,600123,KES,100,0,100,2026-10-13T10:00:00Z,x\n",
	}
	for name, body := range cases {
		if _, err := ParseStatement(strings.NewReader(body)); !errors.Is(err, ErrStatement) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// One bad line rejects the whole file.
	_, err := ParseStatement(strings.NewReader(header +
		"r1,,600123,KES,100,0,100,2026-10-13T10:00:00Z,\n" +
		"r2,,600123,KES,100,0,99,2026-10-13T10:00:00Z,\n"))
	if !errors.Is(err, ErrStatement) || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("partial file: %v", err)
	}
}
