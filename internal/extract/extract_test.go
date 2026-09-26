package extract_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/extract"
	"receipt-autoitemize/internal/itemize"
)

const fixtures = "../../fixtures/task-a"

// Gold mirrors fixtures/task-a/gold.json.
type Gold struct {
	Merchant   string  `json:"merchant"`
	Date       string  `json:"date"`
	Currency   string  `json:"currency"`
	GrandTotal float64 `json:"grand_total"`
	Taxes      []struct {
		Name   string  `json:"name"`
		Rate   float64 `json:"rate"`
		Amount float64 `json:"amount"`
	} `json:"taxes"`
	LineItems []struct {
		Description string  `json:"description"`
		Amount      float64 `json:"amount"`
	} `json:"line_items"`
	ItemizeStatus string `json:"itemize_status"`
}

func loadGold(t *testing.T) map[string]Gold {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, "gold.json"))
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]Gold
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func cents(f float64) domain.Money {
	m, err := domain.ParseMoney(jsonNum(f))
	if err != nil {
		panic(err)
	}
	return m
}

func jsonNum(f float64) string { b, _ := json.Marshal(f); return string(b) }

// TestGold checks header, taxes, auto-itemized lines and status for every fixture.
func TestGold(t *testing.T) {
	for name, want := range loadGold(t) {
		t.Run(name, func(t *testing.T) {
			text, err := os.ReadFile(filepath.Join(fixtures, name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			h := extract.ParseHeader(string(text))
			items := extract.AutoItemize(string(text))

			if h.Merchant == nil || *h.Merchant != want.Merchant {
				t.Errorf("merchant = %v, want %q", h.Merchant, want.Merchant)
			}
			if h.Date == nil || *h.Date != want.Date {
				t.Errorf("date = %v, want %q", h.Date, want.Date)
			}
			if h.Currency == nil || *h.Currency != want.Currency {
				t.Errorf("currency = %v, want %q", h.Currency, want.Currency)
			}
			if h.GrandTotal == nil || *h.GrandTotal != cents(want.GrandTotal) {
				t.Errorf("grand_total = %v, want %v", h.GrandTotal, want.GrandTotal)
			}

			if len(h.Taxes) != len(want.Taxes) {
				t.Fatalf("taxes = %+v, want %+v", h.Taxes, want.Taxes)
			}
			for i, wt := range want.Taxes {
				got := h.Taxes[i]
				if got.Name != wt.Name || got.Amount != cents(wt.Amount) || got.Rate == nil || got.Rate.String() != jsonNum(wt.Rate) {
					t.Errorf("tax[%d] = %+v (rate %v), want %+v", i, got, got.Rate, wt)
				}
			}

			if len(items) != len(want.LineItems) {
				t.Fatalf("items = %+v, want %+v", items, want.LineItems)
			}
			for i, wi := range want.LineItems {
				if items[i].Description != wi.Description || items[i].Amount != cents(wi.Amount) {
					t.Errorf("item[%d] = %+v, want %+v", i, items[i], wi)
				}
			}

			status, _ := itemize.Reconcile(itemize.Input{
				Items: items, Taxes: h.Taxes, GrandTotal: h.GrandTotal, Subtotal: h.Subtotal,
			})
			if string(status) != want.ItemizeStatus {
				t.Errorf("itemize_status = %s, want %s", status, want.ItemizeStatus)
			}
		})
	}
}

func TestTaxVariants(t *testing.T) {
	cases := []struct {
		line      string
		name      string
		rate      string
		inclusive bool
	}{
		{"VAT 19%                     2.85", "VAT", "0.19", false},
		{"incl. VAT 19%               3.83", "VAT", "0.19", true},
		{"MwSt. 7%                    0.70", "VAT", "0.07", false},
		{"inkl. MwSt 19%              1.00", "VAT", "0.19", true},
		{"GST 5,5%                    0.55", "GST", "0.055", false},
		{"Sales tax                   1.20", "SALES_TAX", "", false},
	}
	for _, c := range cases {
		h := extract.ParseHeader(c.line)
		if len(h.Taxes) != 1 {
			t.Errorf("%q: got %d taxes", c.line, len(h.Taxes))
			continue
		}
		tx := h.Taxes[0]
		rate := ""
		if tx.Rate != nil {
			rate = tx.Rate.String()
		}
		if tx.Name != c.name || rate != c.rate || tx.Inclusive != c.inclusive {
			t.Errorf("%q: got %+v rate=%s", c.line, tx, rate)
		}
	}
}

func TestAutoItemizeSkipsNonItems(t *testing.T) {
	text := "Coffee          3.00\nTaxi voucher    5.00\nCash           10.00\nChange          2.00\nTOTAL           8.00\nNo price here\n"
	items := extract.AutoItemize(text)
	if len(items) != 2 || items[0].Description != "Coffee" || items[1].Description != "Taxi voucher" {
		t.Fatalf("items = %+v", items)
	}
}

func TestDates(t *testing.T) {
	for in, want := range map[string]string{"DATE: 2026-03-12": "2026-03-12", "DATE: 12.03.2026": "2026-03-12"} {
		if h := extract.ParseHeader(in); h.Date == nil || *h.Date != want {
			t.Errorf("%q -> %v", in, h.Date)
		}
	}
	if h := extract.ParseHeader("DATE: 03/12/2026"); h.Date != nil {
		t.Errorf("ambiguous slash date should stay unparsed, got %v", *h.Date)
	}
}
