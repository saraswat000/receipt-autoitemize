package itemize

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"receipt-autoitemize/internal/domain"
)

// PATCH can add ~185 max-size items (1 MiB body is plenty). Their true sum is
// ~1.8e17 EUR, but int64 addition wraps in Reconcile, and a crafted last item makes
// the wrapped sum land exactly on 15.00 -> COMPLETE.
func TestReconcileSumOverflowIsNeverComplete(t *testing.T) {
	const maxAmt = domain.Money(99999999999999999) // 999999999999999.99, accepted by ParseMoney
	var ops []Operation
	var wrapped domain.Money
	for i := 0; i < 184; i++ {
		a := maxAmt
		d := fmt.Sprintf("big %d", i)
		ops = append(ops, Operation{Op: "add", Description: &d, Amount: &a})
		wrapped += a // same wrapping arithmetic Reconcile uses
	}
	last := domain.Money(1500) - wrapped
	d := "balancer"
	ops = append(ops, Operation{Op: "add", Description: &d, Amount: &last})
	if _, err := domain.ParseMoney(last.String()); err != nil {
		t.Fatalf("balancer %s must be a valid amount: %v", last, err)
	}
	n := 0
	items, err := Apply(nil, ops, func() string { n++; return fmt.Sprint("li_", n) })
	if err != nil {
		t.Fatal(err)
	}
	gt := domain.Money(1785)
	status, issues := Reconcile(Input{Items: items, GrandTotal: &gt,
		Taxes: []domain.TaxLine{{Name: "VAT", Amount: 285}}})
	if status == domain.ItemizeComplete {
		t.Fatalf("185 items worth ~1.8e17 EUR reconcile against a 17.85 total: status=%s issues=%v", status, issues)
	}
}

// README: a field the op does not take -> 422 UNEXPECTED_FIELD. An empty-but-present
// "item_id" on add/merge slips through because presence is tested as ItemID != "".
func TestEmptyItemIDOnAddIsRejected(t *testing.T) {
	var op Operation
	if err := json.Unmarshal([]byte(`{"op":"add","item_id":"","description":"x","amount":1}`), &op); err != nil {
		t.Fatal(err)
	}
	_, err := op.Command()
	if err == nil || !strings.Contains(err.Error(), "item_id") {
		t.Fatalf(`add with "item_id":"" should be UNEXPECTED_FIELD, got err=%v`, err)
	}
}
