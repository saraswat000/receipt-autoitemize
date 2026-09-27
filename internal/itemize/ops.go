package itemize

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	Op          string          `json:"op"`
	ItemID      string          `json:"item_id,omitempty"`
	ItemIDs     []string        `json:"item_ids,omitempty"`
	Description *string         `json:"description,omitempty"`
	Amount      *domain.Money   `json:"amount,omitempty"`
	Quantity    *domain.Decimal `json:"quantity,omitempty"`
	TaxAmount   *domain.Money   `json:"tax_amount,omitempty"`
	Into        []NewItem       `json:"into,omitempty"`

	// present holds the JSON keys the client actually sent, so a field sent empty
	// ("item_id": "") or null still counts as sent. Nil for ops built in code.
	present map[string]bool
}

// UnmarshalJSON decodes strictly (unknown keys are an error, as for the rest of the
// request) and records which keys were present.
func (o *Operation) UnmarshalJSON(b []byte) error {
	type plain Operation // no methods: avoids recursing into this function
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p plain
	if err := dec.Decode(&p); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	*o = Operation(p)
	o.present = make(map[string]bool, len(keys))
	for k := range keys {
		o.present[k] = true
	}
	return nil
}

// NewItem is an item created by split or add.
type NewItem struct {
	Description string          `json:"description"`
	Amount      *domain.Money   `json:"amount"`
	Quantity    *domain.Decimal `json:"quantity,omitempty"`
	TaxAmount   *domain.Money   `json:"tax_amount,omitempty"`
}

// OpError is a client error in an operation list (unknown item, missing field...).
type OpError struct {
	Index   int    `json:"index"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *OpError) Error() string { return fmt.Sprintf("operation %d: %s", e.Index, e.Message) }

// Command is one executable edit. The wire format is a single Operation struct, but
// each op name maps to its own Command type with its own fields and Execute, so
// adding an op means adding a type and a registry entry, and Apply never grows.
type Command interface {
	Execute(w *worksheet) error
}

type commandSpec struct {
	fields []string // the Operation fields this op accepts, besides "op"
	build  func(Operation) Command
}

// commands maps the wire name of an op to its Command and the fields it takes.
var commands = map[string]commandSpec{
	"update": {[]string{"item_id", "description", "amount", "quantity", "tax_amount"}, func(o Operation) Command { return updateCmd(o) }},
	"merge":  {[]string{"item_ids", "description", "amount"}, func(o Operation) Command { return mergeCmd(o) }},
	"split":  {[]string{"item_id", "into"}, func(o Operation) Command { return splitCmd(o) }},
	"add":    {[]string{"description", "amount", "quantity", "tax_amount"}, func(o Operation) Command { return addCmd(o) }},
	"delete": {[]string{"item_id"}, func(o Operation) Command { return deleteCmd(o) }},
}

// Command decodes the operation into its executable form. A field the op does not
// use is rejected rather than silently ignored ({"op":"delete","amount":5} is a
// client bug, not a delete).
func (o Operation) Command() (Command, error) {
	spec, ok := commands[o.Op]
	if !ok {
		return nil, opFail("UNKNOWN_OP", "unknown op %q (want update, merge, split, add, delete)", o.Op)
	}
	for _, f := range o.setFields() {
		if !slices.Contains(spec.fields, f) {
			return nil, opFail("UNEXPECTED_FIELD", "%s does not take %s", o.Op, f)
		}
	}
	return spec.build(o), nil
}

func (o Operation) setFields() []string {
	var set []string
	if o.present != nil {
		for k := range o.present {
			if k != "op" {
				set = append(set, k)
			}
		}
		slices.Sort(set)
		return set
	}
	for name, isSet := range map[string]bool{
		"item_id": o.ItemID != "", "item_ids": o.ItemIDs != nil, "description": o.Description != nil,
		"amount": o.Amount != nil, "quantity": o.Quantity != nil, "tax_amount": o.TaxAmount != nil, "into": o.Into != nil,
	} {
		if isSet {
			set = append(set, name)
		}
	}
	slices.Sort(set)
	return set
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
			var oe *OpError
			if errors.As(err, &oe) {
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
	if err := validQuantityAndTax(c.Quantity, c.TaxAmount); err != nil {
		return opFail("INVALID_ITEM", "%s", err)
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
		if next, ok := addChecked(sum, w.items[i].Amount, true); ok {
			sum = next
		} else {
			return opFail("AMOUNT_TOO_LARGE", "merged amount is too large")
		}
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
	if err := validQuantityAndTax(p.Quantity, p.TaxAmount); err != nil {
		return domain.LineItem{}, err
	}
	return domain.LineItem{
		ID: newID(), Description: strings.TrimSpace(p.Description), Amount: *p.Amount,
		Quantity: p.Quantity, TaxAmount: p.TaxAmount, Source: domain.SourceUser,
	}, nil
}

// validQuantityAndTax: amounts may be negative (discounts, refunds), but a
// quantity must be positive and an item's tax cannot be negative.
func validQuantityAndTax(qty *domain.Decimal, tax *domain.Money) error {
	if qty != nil && qty.Sign() <= 0 {
		return fmt.Errorf("quantity must be greater than zero")
	}
	if tax != nil && *tax < 0 {
		return fmt.Errorf("tax_amount must not be negative")
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
