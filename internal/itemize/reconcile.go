// Package itemize holds the business rules for line items: when they reconcile with
// the receipt, and how user edits (edit / merge / split / add / delete) are applied.
package itemize

import "receipt-autoitemize/internal/domain"

// Tolerance is the rounding slack allowed when reconciling (one cent).
const Tolerance domain.Money = 1

// Input is what reconciliation needs. Subtotal is optional; when present it is
// checked too, which catches OCR that dropped or duplicated an item line.
type Input struct {
	Items      []domain.LineItem
	Taxes      []domain.TaxLine
	GrandTotal *domain.Money
	Subtotal   *domain.Money
	// Adjustments are items printed after the subtotal (tip, rounding): they count
	// toward the total but are left out of the subtotal check.
	Adjustments domain.Money
}

// Reconcile decides the itemize status and explains it.
//
// Items are NET of any exclusive (added-on) tax, so the rule is
//
//	sum(items) + sum(exclusive taxes) == grand_total   (± Tolerance)
//
// Inclusive taxes ("incl. VAT 19%") are already inside the prices and are not added.
// Reconcile never changes the total and never invents a balancing line: a mismatch
// is reported, not fixed.
func Reconcile(in Input) (domain.ItemizeStatus, []domain.Issue) {
	if in.GrandTotal == nil {
		return domain.ItemizeFailed, []domain.Issue{{
			Code: "NO_TOTAL", Message: "No grand total found in the receipt text.",
		}}
	}
	if len(in.Items) == 0 {
		return domain.ItemizeNeedsReview, []domain.Issue{{
			Code: "NO_ITEMS", Message: "No reliable line items found on the receipt.",
		}}
	}

	var itemsSum, addedTax domain.Money
	ok := true
	for _, it := range in.Items {
		itemsSum, ok = addChecked(itemsSum, it.Amount, ok)
	}
	for _, t := range in.Taxes {
		if !t.Inclusive {
			addedTax, ok = addChecked(addedTax, t.Amount, ok)
		}
	}
	computed, ok := addChecked(itemsSum, addedTax, ok)
	if !ok {
		// int64 wrapped: a wrapped sum could land on the total by accident.
		return domain.ItemizeFailed, []domain.Issue{{
			Code: "AMOUNT_OVERFLOW", Message: "Line item amounts are too large to add up.",
		}}
	}

	var issues []domain.Issue
	if diff := *in.GrandTotal - computed; abs(diff) > Tolerance {
		issues = append(issues, domain.Issue{
			Code:          "TOTAL_MISMATCH",
			Message:       "Line items plus added taxes do not equal the receipt total.",
			ItemsTotal:    domain.MoneyPtr(itemsSum),
			AddedTaxes:    domain.MoneyPtr(addedTax),
			ComputedTotal: domain.MoneyPtr(computed),
			GrandTotal:    domain.MoneyPtr(*in.GrandTotal),
			Difference:    domain.MoneyPtr(diff),
		})
	}
	if in.Subtotal != nil {
		if diff := *in.Subtotal - (itemsSum - in.Adjustments); abs(diff) > Tolerance {
			issues = append(issues, domain.Issue{
				Code:       "SUBTOTAL_MISMATCH",
				Message:    "Line items do not equal the printed subtotal.",
				ItemsTotal: domain.MoneyPtr(itemsSum),
				Subtotal:   domain.MoneyPtr(*in.Subtotal),
				Difference: domain.MoneyPtr(diff),
			})
		}
	}
	if len(issues) > 0 {
		return domain.ItemizeNeedsReview, issues
	}
	return domain.ItemizeComplete, []domain.Issue{}
}

// addChecked adds b to a, reporting false (sticky) once the int64 sum overflows.
func addChecked(a, b domain.Money, ok bool) (domain.Money, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return s, false
	}
	return s, ok
}

func abs(m domain.Money) domain.Money {
	if m < 0 {
		return -m
	}
	return m
}
