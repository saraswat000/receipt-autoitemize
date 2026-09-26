package itemize

import (
	"errors"
	"fmt"
	"testing"

	"receipt-autoitemize/internal/domain"
)

func m(v domain.Money) *domain.Money { return &v }
func s(v string) *string             { return &v }

func items(amounts ...domain.Money) []domain.LineItem {
	out := make([]domain.LineItem, len(amounts))
	for i, a := range amounts {
		out[i] = domain.LineItem{ID: fmt.Sprintf("li_%d", i+1), Description: fmt.Sprintf("item %d", i+1), Amount: a, Source: domain.SourceAuto}
	}
	return out
}

var addedVAT = []domain.TaxLine{{Name: "VAT", Amount: 285}}
var inclVAT = []domain.TaxLine{{Name: "VAT", Amount: 383, Inclusive: true}}

func TestReconcile(t *testing.T) {
	cases := []struct {
		name   string
		in     Input
		status domain.ItemizeStatus
		codes  []string
	}{
		{"net items + added tax", Input{items(350, 890, 260), addedVAT, m(1785), m(1500)}, domain.ItemizeComplete, nil},
		{"one cent rounding tolerated", Input{items(350, 890, 259), addedVAT, m(1785), nil}, domain.ItemizeComplete, nil},
		{"two cents is a mismatch", Input{items(350, 890, 258), addedVAT, m(1785), nil}, domain.ItemizeNeedsReview, []string{"TOTAL_MISMATCH"}},
		{"gross items + inclusive tax", Input{items(2400), inclVAT, m(2400), nil}, domain.ItemizeComplete, nil},
		{"no items", Input{nil, inclVAT, m(2400), nil}, domain.ItemizeNeedsReview, []string{"NO_ITEMS"}},
		{"no total", Input{items(100), nil, nil, nil}, domain.ItemizeFailed, []string{"NO_TOTAL"}},
		{"mismatch fixture", Input{items(400, 600), []domain.TaxLine{{Name: "VAT", Amount: 190}}, m(1850), m(1000)}, domain.ItemizeNeedsReview, []string{"TOTAL_MISMATCH"}},
		{"subtotal disagrees", Input{items(350, 890), addedVAT, m(1525), m(1500)}, domain.ItemizeNeedsReview, []string{"SUBTOTAL_MISMATCH"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, issues := Reconcile(c.in)
			if status != c.status {
				t.Fatalf("status = %s, want %s (%+v)", status, c.status, issues)
			}
			if len(issues) != len(c.codes) {
				t.Fatalf("issues = %+v, want codes %v", issues, c.codes)
			}
			for i, code := range c.codes {
				if issues[i].Code != code {
					t.Errorf("issue[%d] = %s, want %s", i, issues[i].Code, code)
				}
			}
		})
	}
}

func TestReconcileMismatchDetails(t *testing.T) {
	_, issues := Reconcile(Input{items(400, 600), []domain.TaxLine{{Amount: 190}}, m(1850), nil})
	got := issues[0]
	if *got.ComputedTotal != 1190 || *got.GrandTotal != 1850 || *got.Difference != 660 {
		t.Fatalf("details = %+v", got)
	}
}

func TestApply(t *testing.T) {
	seq := 0
	newID := func() string { seq++; return fmt.Sprintf("new_%d", seq) }
	base := items(350, 890, 260)

	t.Run("update", func(t *testing.T) {
		got, err := Apply(base, []Operation{{Op: "update", ItemID: "li_1", Description: s("Latte"), Amount: m(400)}}, newID)
		if err != nil || got[0].Description != "Latte" || got[0].Amount != 400 || got[0].Source != domain.SourceUser {
			t.Fatalf("%+v %v", got, err)
		}
		if base[0].Description != "item 1" {
			t.Fatal("input was mutated")
		}
	})
	t.Run("merge keeps position and sums", func(t *testing.T) {
		got, err := Apply(base, []Operation{{Op: "merge", ItemIDs: []string{"li_3", "li_1"}, Description: s("Drinks")}}, newID)
		if err != nil || len(got) != 2 || got[0].Description != "Drinks" || got[0].Amount != 610 || got[1].ID != "li_2" {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("split in place", func(t *testing.T) {
		got, err := Apply(base, []Operation{{Op: "split", ItemID: "li_2", Into: []NewItem{
			{Description: "Bread", Amount: m(440)}, {Description: "Filling", Amount: m(450)},
		}}}, newID)
		if err != nil || len(got) != 4 || got[1].Description != "Bread" || got[2].Description != "Filling" || got[3].ID != "li_3" {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("add and delete", func(t *testing.T) {
		got, err := Apply(base, []Operation{
			{Op: "delete", ItemID: "li_1"},
			{Op: "add", Description: s("Tip"), Amount: m(100)},
		}, newID)
		if err != nil || len(got) != 3 || got[0].ID != "li_2" || got[2].Description != "Tip" {
			t.Fatalf("%+v %v", got, err)
		}
	})
	t.Run("ops see earlier ops", func(t *testing.T) {
		got, err := Apply(base, []Operation{
			{Op: "merge", ItemIDs: []string{"li_1", "li_2"}, Description: s("Food")},
			{Op: "delete", ItemID: "li_1"},
		}, newID)
		var opErr *OpError
		if !errors.As(err, &opErr) || opErr.Index != 1 || opErr.Code != "UNKNOWN_ITEM" {
			t.Fatalf("%+v %v", got, err)
		}
	})

	bad := []struct {
		name string
		op   Operation
		code string
	}{
		{"unknown op", Operation{Op: "explode"}, "UNKNOWN_OP"},
		{"unknown item", Operation{Op: "delete", ItemID: "nope"}, "UNKNOWN_ITEM"},
		{"empty update", Operation{Op: "update", ItemID: "li_1"}, "EMPTY_UPDATE"},
		{"blank description", Operation{Op: "update", ItemID: "li_1", Description: s("  ")}, "INVALID_DESCRIPTION"},
		{"merge one", Operation{Op: "merge", ItemIDs: []string{"li_1"}, Description: s("x")}, "INVALID_MERGE"},
		{"merge dup", Operation{Op: "merge", ItemIDs: []string{"li_1", "li_1"}, Description: s("x")}, "DUPLICATE_ITEM"},
		{"split one", Operation{Op: "split", ItemID: "li_1", Into: []NewItem{{Description: "a", Amount: m(1)}}}, "INVALID_SPLIT"},
		{"split no amount", Operation{Op: "split", ItemID: "li_1", Into: []NewItem{{Description: "a"}, {Description: "b", Amount: m(1)}}}, "INVALID_ITEM"},
		{"add no description", Operation{Op: "add", Amount: m(1)}, "INVALID_ITEM"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := Apply(base, []Operation{c.op}, newID)
			var opErr *OpError
			if !errors.As(err, &opErr) || opErr.Code != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
		})
	}
}
