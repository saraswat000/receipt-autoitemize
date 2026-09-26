package itemize

import (
	"fmt"
	"strings"

	"receipt-autoitemize/internal/domain"
)

// Operation is one user edit in PATCH /transactions/{id}/items. Fields that do not
// apply to the op must be omitted.
//
//	{"op":"update","item_id":"li_1","description":"Latte","amount":"3.90"}
//	{"op":"merge","item_ids":["li_1","li_3"],"description":"Drinks"}           // amount defaults to the sum
//	{"op":"split","item_id":"li_2","into":[{"description":"A","amount":4.4},{"description":"B","amount":4.5}]}
//	{"op":"add","description":"Tip","amount":1.00}
//	{"op":"delete","item_id":"li_3"}
type Operation struct {
	Op          string        `json:"op"`
	ItemID      string        `json:"item_id,omitempty"`
	ItemIDs     []string      `json:"item_ids,omitempty"`
	Description *string       `json:"description,omitempty"`
	Amount      *domain.Money `json:"amount,omitempty"`
	Quantity    *float64      `json:"quantity,omitempty"`
	TaxAmount   *domain.Money `json:"tax_amount,omitempty"`
	Into        []NewItem     `json:"into,omitempty"`
}

// NewItem is an item created by split or add.
type NewItem struct {
	Description string        `json:"description"`
	Amount      *domain.Money `json:"amount"`
	Quantity    *float64      `json:"quantity,omitempty"`
	TaxAmount   *domain.Money `json:"tax_amount,omitempty"`
}

// OpError is a client error in an operation list (unknown item, missing field...).
type OpError struct {
	Index   int    `json:"index"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *OpError) Error() string { return fmt.Sprintf("operation %d: %s", e.Index, e.Message) }

// Apply runs the operations in order on a copy of items and returns the result.
// It is all-or-nothing: the first invalid operation aborts with an *OpError and the
// input slice is never modified. newID mints IDs for items created by merge/split/add.
func Apply(items []domain.LineItem, ops []Operation, newID func() string) ([]domain.LineItem, error) {
	out := make([]domain.LineItem, len(items))
	copy(out, items)

	for n, op := range ops {
		fail := func(code, format string, args ...any) error {
			return &OpError{Index: n, Code: code, Message: fmt.Sprintf(format, args...)}
		}
		find := func(id string) (int, error) {
			for i, it := range out {
				if it.ID == id {
					return i, nil
				}
			}
			return -1, fail("UNKNOWN_ITEM", "no line item %q on this transaction", id)
		}
		validDesc := func(d *string) bool { return d != nil && strings.TrimSpace(*d) != "" }

		switch op.Op {
		case "update":
			i, err := find(op.ItemID)
			if err != nil {
				return nil, err
			}
			if op.Description == nil && op.Amount == nil && op.Quantity == nil && op.TaxAmount == nil {
				return nil, fail("EMPTY_UPDATE", "update needs at least one of description, amount, quantity, tax_amount")
			}
			if op.Description != nil {
				if !validDesc(op.Description) {
					return nil, fail("INVALID_DESCRIPTION", "description must not be empty")
				}
				out[i].Description = strings.TrimSpace(*op.Description)
			}
			if op.Amount != nil {
				out[i].Amount = *op.Amount
			}
			if op.Quantity != nil {
				out[i].Quantity = op.Quantity
			}
			if op.TaxAmount != nil {
				out[i].TaxAmount = op.TaxAmount
			}
			out[i].Source = domain.SourceUser

		case "merge":
			if len(op.ItemIDs) < 2 {
				return nil, fail("INVALID_MERGE", "merge needs at least two item_ids")
			}
			if !validDesc(op.Description) {
				return nil, fail("INVALID_DESCRIPTION", "merge needs a description")
			}
			seen := map[string]bool{}
			var idx []int
			var sum domain.Money
			var tax *domain.Money
			for _, id := range op.ItemIDs {
				if seen[id] {
					return nil, fail("DUPLICATE_ITEM", "item %q listed twice", id)
				}
				seen[id] = true
				i, err := find(id)
				if err != nil {
					return nil, err
				}
				idx = append(idx, i)
				sum += out[i].Amount
				if out[i].TaxAmount != nil {
					t := *out[i].TaxAmount
					if tax != nil {
						t += *tax
					}
					tax = &t
				}
			}
			if op.Amount != nil {
				sum = *op.Amount
			}
			merged := domain.LineItem{
				ID: newID(), Description: strings.TrimSpace(*op.Description),
				Amount: sum, TaxAmount: tax, Source: domain.SourceUser,
			}
			first := minInt(idx)
			var next []domain.LineItem
			for i, it := range out {
				if i == first {
					next = append(next, merged)
				}
				if !seen[it.ID] {
					next = append(next, it)
				}
			}
			out = next

		case "split":
			i, err := find(op.ItemID)
			if err != nil {
				return nil, err
			}
			if len(op.Into) < 2 {
				return nil, fail("INVALID_SPLIT", "split needs at least two parts in into")
			}
			parts := make([]domain.LineItem, 0, len(op.Into))
			for _, p := range op.Into {
				item, err := newItem(p, newID)
				if err != nil {
					return nil, fail("INVALID_ITEM", "%s", err)
				}
				parts = append(parts, item)
			}
			out = append(out[:i], append(parts, out[i+1:]...)...)

		case "add":
			item, err := newItem(NewItem{
				Description: deref(op.Description), Amount: op.Amount,
				Quantity: op.Quantity, TaxAmount: op.TaxAmount,
			}, newID)
			if err != nil {
				return nil, fail("INVALID_ITEM", "%s", err)
			}
			out = append(out, item)

		case "delete":
			i, err := find(op.ItemID)
			if err != nil {
				return nil, err
			}
			out = append(out[:i], out[i+1:]...)

		default:
			return nil, fail("UNKNOWN_OP", "unknown op %q (want update, merge, split, add, delete)", op.Op)
		}
	}
	return out, nil
}

func newItem(p NewItem, newID func() string) (domain.LineItem, error) {
	if strings.TrimSpace(p.Description) == "" {
		return domain.LineItem{}, fmt.Errorf("description is required")
	}
	if p.Amount == nil {
		return domain.LineItem{}, fmt.Errorf("amount is required")
	}
	return domain.LineItem{
		ID: newID(), Description: strings.TrimSpace(p.Description), Amount: *p.Amount,
		Quantity: p.Quantity, TaxAmount: p.TaxAmount, Source: domain.SourceUser,
	}, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func minInt(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}
