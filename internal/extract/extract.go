// Package extract turns raw OCR text into structured receipt fields and proposes
// line items (auto-itemize). It is pure: no I/O, no clock, no IDs.
package extract

import (
	"regexp"
	"strings"
	"time"

	"receipt-autoitemize/internal/domain"
)

var (
	headerRE = regexp.MustCompile(`(?i)^\s*(MERCHANT|DATE|CURRENCY)\s*:\s*(.+?)\s*$`)
	// "<label><2+ spaces><amount>", e.g. "Mineral water               2.60".
	amountLineRE = regexp.MustCompile(`^\s*(\S.*?)\s{2,}(-?\d+[.,]\d{2})\s*$`)
	// "VAT 19%", "incl. VAT 19%", "19% VAT", "Total VAT", "TOTAL TAX".
	taxRE = regexp.MustCompile(
		`(?i)^(?P<total>total\s+)?(?P<incl>incl\.?|including|inkl\.?)?\s*` +
			`(?:(?P<pre>\d+(?:[.,]\d+)?)\s*%\s*)?` +
			`(?P<name>VAT|GST|HST|PST|QST|MwSt|USt|Sales tax|Tax)\b\.?\s*` +
			`(?:(?P<post>\d+(?:[.,]\d+)?)\s*%)?`)
	totalRE    = regexp.MustCompile(`(?i)^(grand\s+)?total\b|^amount\s+due\b|^summe\b`)
	subtotalRE = regexp.MustCompile(`(?i)^sub\s*-?\s*total\b|^net\b|^zwischensumme\b|^netto\b`)
	// Payment / change lines carry an amount but are not purchases.
	nonItemRE = regexp.MustCompile(`(?i)^(cash|card|visa|mastercard|amex|change|` +
		`bar|r(?:ü|ue)ckgeld|gegeben|ec-karte|karte|kartenzahlung)\b`)
	// Tip and rounding are charged, so they are items that count toward the total,
	// but they come after the printed subtotal, so the subtotal check leaves them out.
	adjustmentRE = regexp.MustCompile(`(?i)^(tip|gratuity|service charge|rounding|trinkgeld|rundung)\b`)
	currencyRE   = regexp.MustCompile(`^[A-Z]{3}$`)
)

var taxNameAliases = map[string]string{"MWST": "VAT", "UST": "VAT", "SALES TAX": "SALES_TAX"}

// Header is everything on the receipt except the line items.
type Header struct {
	Merchant   *string
	Date       *string // normalised to YYYY-MM-DD
	Currency   *string // ISO 4217
	Subtotal   *domain.Money
	GrandTotal *domain.Money
	Taxes      []domain.TaxLine
	// Adjustments is the sum of tip / rounding lines. They are line items, but not
	// part of the printed subtotal.
	Adjustments domain.Money
}

type amountLine struct {
	label  string
	amount domain.Money
}

func lines(text string) []string {
	// A UTF-8 byte-order mark (Windows editors add one) would hide the first line.
	return strings.Split(strings.TrimPrefix(text, "\ufeff"), "\n")
}

func amountLines(text string) []amountLine {
	var out []amountLine
	for _, line := range lines(text) {
		m := amountLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		amt, err := domain.ParseMoney(m[2])
		if err != nil {
			continue
		}
		out = append(out, amountLine{label: strings.TrimSpace(m[1]), amount: amt})
	}
	return out
}

// ParseHeader extracts merchant, date, currency, subtotal, grand total and taxes.
func ParseHeader(text string) Header {
	var h Header
	for _, line := range lines(text) {
		m := headerRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		val := m[2]
		switch strings.ToUpper(m[1]) {
		case "MERCHANT":
			h.Merchant = &val
		case "DATE":
			h.Date = normaliseDate(val)
		case "CURRENCY":
			if c := strings.ToUpper(val); currencyRE.MatchString(c) {
				h.Currency = &c
			}
		}
	}

	for _, l := range amountLines(text) {
		// Tax before total: "Total VAT 2.85" is a tax line, not the grand total.
		if tax, ok := parseTax(l); ok {
			h.Taxes = append(h.Taxes, tax)
			continue
		}
		switch {
		case subtotalRE.MatchString(l.label):
			h.Subtotal = domain.MoneyPtr(l.amount)
		case totalRE.MatchString(l.label):
			h.GrandTotal = domain.MoneyPtr(l.amount)
		case adjustmentRE.MatchString(l.label):
			h.Adjustments += l.amount
		}
	}
	return h
}

func parseTax(l amountLine) (domain.TaxLine, bool) {
	m := taxRE.FindStringSubmatch(l.label)
	if m == nil {
		return domain.TaxLine{}, false
	}
	g := func(name string) string { return m[taxRE.SubexpIndex(name)] }
	if g("total") != "" && g("incl") != "" {
		return domain.TaxLine{}, false // "Total incl. VAT 17.85" is the grand total
	}
	name := strings.ToUpper(g("name"))
	if alias, ok := taxNameAliases[name]; ok {
		name = alias
	}
	tax := domain.TaxLine{Name: name, Amount: l.amount, Inclusive: g("incl") != ""}
	rate := g("post")
	if rate == "" {
		rate = g("pre")
	}
	if rate != "" {
		if r, err := domain.ParseRatePercent(rate); err == nil {
			tax.Rate = &r
		}
	}
	return tax, true
}

// AutoItemize proposes line items: every priced line that is not a subtotal, total,
// tax or payment line. Lines without a price ("Trip fare") are not reliable items and
// are skipped, so a receipt like the taxi one yields no items and goes to review.
func AutoItemize(text string) []domain.LineItem {
	items := []domain.LineItem{}
	for _, l := range amountLines(text) {
		if _, isTax := parseTax(l); isTax {
			continue
		}
		if subtotalRE.MatchString(l.label) || totalRE.MatchString(l.label) || nonItemRE.MatchString(l.label) {
			continue
		}
		items = append(items, domain.LineItem{
			Description: l.label,
			Amount:      l.amount,
			Source:      domain.SourceAuto,
		})
	}
	return items
}

// normaliseDate accepts ISO (2026-03-12) and European day-first (12.03.2026) dates.
// Slash dates are ambiguous (US vs EU) and are left unparsed rather than guessed.
func normaliseDate(s string) *string {
	for _, layout := range []string{"2006-01-02", "02.01.2006", "2.1.2006"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			iso := t.Format("2006-01-02")
			return &iso
		}
	}
	return nil
}
