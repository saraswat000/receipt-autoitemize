package extract_test

import (
	"testing"

	"receipt-autoitemize/internal/domain"
	"receipt-autoitemize/internal/extract"
	"receipt-autoitemize/internal/itemize"
)

func run(text string) (extract.Header, []domain.LineItem, domain.ItemizeStatus, []domain.Issue) {
	h := extract.ParseHeader(text)
	items := extract.AutoItemize(text)
	st, is := itemize.Reconcile(itemize.Input{Items: items, Taxes: h.Taxes, GrandTotal: h.GrandTotal, Subtotal: h.Subtotal, Adjustments: h.Adjustments})
	return h, items, st, is
}

// "Total VAT" / "Total tax" matches totalRE (^total\b) before the tax branch is tried:
// the tax is lost, and if the line comes after TOTAL it overwrites the grand total.
func TestTotalVATLineIsATax(t *testing.T) {
	text := "MERCHANT: Cafe Mitte\n\nEspresso        3.50\nSandwich        8.90\nMineral water   2.60\n\n" +
		"Subtotal       15.00\nTOTAL          17.85\nTotal VAT 19%   2.85\n"
	h, _, st, is := run(text)
	if h.GrandTotal == nil || *h.GrandTotal != 1785 {
		t.Errorf("grand total = %v, want 17.85", h.GrandTotal)
	}
	if len(h.Taxes) != 1 || h.Taxes[0].Amount != 285 {
		t.Errorf("taxes = %+v, want one VAT 2.85", h.Taxes)
	}
	if st != domain.ItemizeComplete {
		t.Errorf("status = %s %+v, want COMPLETE", st, is)
	}
}

// Same root cause, US layout: TOTAL TAX before TOTAL. Tax silently dropped.
func TestTotalTaxLineIsATax(t *testing.T) {
	text := "Burger          10.00\nFries            5.00\nSUBTOTAL        15.00\nTOTAL TAX        1.24\nTOTAL           16.24\n"
	h, _, st, is := run(text)
	if len(h.Taxes) != 1 {
		t.Errorf("taxes = %+v, want the TOTAL TAX line", h.Taxes)
	}
	if st != domain.ItemizeComplete {
		t.Errorf("status = %s %+v, want COMPLETE", st, is)
	}
}

// German receipts are explicitly supported (Summe, MwSt, inkl.). But German summary
// and payment lines become purchased items.
func TestGermanSummaryAndPaymentLinesAreNotItems(t *testing.T) {
	text := "MERCHANT: Baeckerei\nDATE: 12.03.2026\nCURRENCY: EUR\n\n" +
		"Brezel           2,00\nKaffee           3,00\n" +
		"Zwischensumme    5,00\nSumme            5,00\ninkl. MwSt 7%    0,33\n" +
		"Bar             10,00\nRückgeld         5,00\n"
	_, items, st, is := run(text)
	var descs []string
	for _, it := range items {
		descs = append(descs, it.Description)
	}
	if len(items) != 2 {
		t.Errorf("items = %q, want [Brezel Kaffee]", descs)
	}
	if st != domain.ItemizeComplete {
		t.Errorf("status = %s %+v, want COMPLETE", st, is)
	}
}

// Tip and rounding are part of the amount charged (unlike cash/change), and ops.go
// itself documents {"op":"add","description":"Tip"} as an item. AutoItemize drops them,
// so a correct tipped receipt can never auto-reconcile.
func TestTipLineCountsTowardTheTotal(t *testing.T) {
	text := "Pasta           12.00\nWine             6.00\nSubtotal        18.00\nTip              2.00\nTOTAL           20.00\n"
	_, items, st, is := run(text)
	if st != domain.ItemizeComplete {
		t.Errorf("status = %s %+v (items %d), want COMPLETE", st, is, len(items))
	}
}

// A rate printed before the tax name makes the tax line an item and no tax is recorded;
// with no subtotal line, the result is COMPLETE with the VAT booked as a purchase.
func TestRateFirstTaxLineIsATax(t *testing.T) {
	text := "Espresso         3.50\nSandwich        11.50\n19% VAT          2.85\nTOTAL           17.85\n"
	h, items, st, _ := run(text)
	if len(h.Taxes) != 1 {
		t.Errorf("taxes = %+v, want the 19%% VAT line; items=%+v status=%s", h.Taxes, items, st)
	}
}

// Guard for the tax-before-total order: a total that merely mentions included tax
// is still the grand total, and the gold-style "incl. VAT" line is still a tax.
func TestTotalInclVATIsTheGrandTotal(t *testing.T) {
	h, _, _, _ := run("Taxi fare       20.00\nTotal incl. VAT  20.00\nincl. VAT 19%    3.19\n")
	if h.GrandTotal == nil || *h.GrandTotal != 2000 {
		t.Errorf("grand total = %v, want 20.00", h.GrandTotal)
	}
	if len(h.Taxes) != 1 || !h.Taxes[0].Inclusive || h.Taxes[0].Rate == nil || h.Taxes[0].Rate.String() != "0.19" {
		t.Errorf("taxes = %+v, want one inclusive 19%% VAT", h.Taxes)
	}
}

func TestRateFirstTaxKeepsRate(t *testing.T) {
	h, _, _, _ := run("Espresso         3.50\n7% MwSt          0.25\nSumme            3.75\n")
	if len(h.Taxes) != 1 || h.Taxes[0].Name != "VAT" || h.Taxes[0].Rate == nil || h.Taxes[0].Rate.String() != "0.07" {
		t.Fatalf("taxes = %+v", h.Taxes)
	}
}

// Rates with more than two decimals of a percent (US sales tax 8.875%) are kept
// exactly instead of being dropped.
func TestThreeDecimalRatesAreKept(t *testing.T) {
	for line, want := range map[string]string{
		"Sales tax 8.875%   1.33\n": "0.08875",
		"VAT 19.123%        2.87\n": "0.19123",
	} {
		h := extract.ParseHeader(line)
		if len(h.Taxes) != 1 || h.Taxes[0].Rate == nil || h.Taxes[0].Rate.String() != want {
			t.Errorf("%q: taxes = %+v, want rate %s", line, h.Taxes, want)
		}
	}
}
