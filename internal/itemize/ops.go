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

// Command is one executable edit. Each op in the request decodes into its own
// Command type (the Command pattern), so adding an op means adding a type and a
// registry entry, and Apply never grows.
type Command interface {
	Execute(w *worksheet) error
}

// commands maps the wire name of an op to the constructor of its Command.
var commands = map[string]func(Operation) Command{
	"update": func(o Operation) Command { return updateCmd(o) },
	"merge":  func(o Operation) Command { return mergeCmd(o) },
	"split":  func(o Operation) Command { return splitCmd(o) },
	"add":    func(o Operation) Command { return addCmd(o) },
	"delete": func(o Operation) Command { return deleteCmd(o) },
}

// Command decodes the operation into its executable form.
func (o Operation) Command() (Command, error) {
	mk, ok := commands[o.Op]
	if !ok {
		return nil, opFail("UNKNOWN_OP", "unknown op %q (want update, merge, split, add, delete)", o.Op)
	}
	return mk(o), nil
}

// worksheet is the working copy the commands edit.
type worksheet struct {
	items []domain.LineItem
	newID func() string
}

func (w *worksheet) find(id string) (int, error) {
	for i, it := range w.items {
		if it.ID == id {
			return i, nil
		}
	}
	return -1, opFail("UNKNOWN_ITEM", "no line item %q on this transaction", id)
}

// Apply runs the operations in order on a copy of items and returns the result.
// It is all-or-nothing: the first invalid operation aborts with an *OpError and the
// input slice is never modified. newID mints IDs for items created by merge/split/add.
func Apply(items []domain.LineItem, ops []Operation, newID func() string) ([]domain.LineItem, error) {
	w := &worksheet{items: append([]domain.LineItem(nil), items...), newID: newID}
	if w.items == nil {
		w.items = []domain.LineItem{}
	}
	for n, op := range ops {
		cmd, err := op.Command()
		if err == nil {
			err = cmd.Execute(w)
		}
		if err != nil {
			if oe, ok := err.(*OpError); ok {
				oe.Index = n
			}
			return nil, err
		}
	}
	return w.items, nil
}

type (
	updateCmd Operation
	mergeCmd  Operation
	splitCmd  Operation
	addCmd    Operation
	deleteCmd Operation
)

func (c updateCmd) Execute(w *worksheet) error {
	i, err := w.find(c.ItemID)
	if err != nil {
		return err
	}
	if c.Description == nil && c.Amount == nil && c.Quantity == nil && c.TaxAmount == nil {
		return opFail("EMPTY_UPDATE", "update needs at least one of description, amount, quantity, tax_amount")
	}
	it := &w.items[i]
	if c.Description != nil {
		if !validDesc(c.Description) {
			return opFail("INVALID_DESCRIPTION", "description must not be empty")
		}
		it.Description = strings.TrimSpace(*c.Description)
	}
	if c.Amount != nil {
		it.Amount = *c.Amount
	}
	if c.Quantity != nil {
		it.Quantity = c.Quantity
	}
	if c.TaxAmount != nil {
		it.TaxAmount = c.TaxAmount
	}
	it.Source = domain.SourceUser
	return nil
}

// Execute replaces the listed items with one item at the position of the first of
// them. The amount defaults to the sum; tax_amounts are summed when present.
func (c mergeCmd) Execute(w *worksheet) error {
	if len(c.ItemIDs) < 2 {
		return opFail("INVALID_MERGE", "merge needs at least two item_ids")
	}
	if !validDesc(c.Description) {
		return opFail("INVALID_DESCRIPTION", "merge needs a description")
	}
	seen := map[string]bool{}
	first := len(w.items)
	var sum domain.Money
	var tax *domain.Money
	for _, id := range c.ItemIDs {
		if seen[id] {
			return opFail("DUPLICATE_ITEM", "item %q listed twice", id)
		}
		seen[id] = true
		i, err := w.find(id)
		if err != nil {
			return err
		}
		first = min(first, i)
		sum += w.items[i].Amount
		if t := w.items[i].TaxAmount; t != nil {
			total := *t
			if tax != nil {
				total += *tax
			}
			tax = &total
		}
	}
	if c.Amount != nil {
		sum = *c.Amount
	}
	merged := domain.LineItem{
		ID: w.newID(), Description: strings.TrimSpace(*c.Description),
		Amount: sum, TaxAmount: tax, Source: domain.SourceUser,
	}
	next := make([]domain.LineItem, 0, len(w.items)-len(c.ItemIDs)+1)
	for i, it := range w.items {
		if i == first {
			next = append(next, merged)
		}
		if !seen[it.ID] {
			next = append(next, it)
		}
	}
	w.items = next
	return nil
}

func (c splitCmd) Execute(w *worksheet) error {
	i, err := w.find(c.ItemID)
	if err != nil {
		return err
	}
	if len(c.Into) < 2 {
		return opFail("INVALID_SPLIT", "split needs at least two parts in into")
	}
	parts := make([]domain.LineItem, 0, len(c.Into))
	for _, p := range c.Into {
		item, err := newItem(p, w.newID)
		if err != nil {
			return opFail("INVALID_ITEM", "%s", err)
		}
		parts = append(parts, item)
	}
	w.items = append(w.items[:i], append(parts, w.items[i+1:]...)...)
	return nil
}

func (c addCmd) Execute(w *worksheet) error {
	item, err := newItem(NewItem{
		Description: deref(c.Description), Amount: c.Amount,
		Quantity: c.Quantity, TaxAmount: c.TaxAmount,
	}, w.newID)
	if err != nil {
		return opFail("INVALID_ITEM", "%s", err)
	}
	w.items = append(w.items, item)
	return nil
}

func (c deleteCmd) Execute(w *worksheet) error {
	i, err := w.find(c.ItemID)
	if err != nil {
		return err
	}
	w.items = append(w.items[:i], w.items[i+1:]...)
	return nil
}

// opFail builds an *OpError; Apply fills in the index of the failing operation.
func opFail(code, format string, args ...any) error {
	return &OpError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func validDesc(d *string) bool { return d != nil && strings.TrimSpace(*d) != "" }

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
