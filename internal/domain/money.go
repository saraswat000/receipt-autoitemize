// Package domain holds the core types shared by every layer: money, rates,
// receipts, transactions, taxes and line items.
package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Money is an amount in minor units (cents). Floats never touch money: 15.00 + 2.85
// must equal 17.85 exactly when we reconcile. The service assumes 2-decimal
// currencies (EUR, USD, ...); see ARCHITECTURE.md for zero/three-decimal currencies.
type Money int64

// ParseMoney parses "3.50", "3,50", "-1.9" or "24" into cents. More than two
// fractional digits is rejected rather than silently rounded.
func ParseMoney(s string) (Money, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" && !hasFrac || len(frac) > 2 || (hasFrac && frac == "") {
		return 0, fmt.Errorf("invalid amount %q (at most 2 decimals)", s)
	}
	if whole == "" {
		whole = "0"
	}
	for len(frac) < 2 {
		frac += "0"
	}
	if !isDigits(whole) || !isDigits(frac) {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	m := Money(w*100 + f)
	if neg {
		m = -m
	}
	return m, nil
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// String renders the amount with exactly two decimals: 1785 -> "17.85".
func (m Money) String() string {
	sign := ""
	v := int64(m)
	if v < 0 {
		sign, v = "-", -v
	}
	return fmt.Sprintf("%s%d.%02d", sign, v/100, v%100)
}

// MarshalJSON emits a JSON number (17.85), matching the shape of gold.json.
func (m Money) MarshalJSON() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalJSON accepts a JSON number or a numeric string, parsed exactly.
func (m *Money) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = []byte(s)
	}
	v, err := ParseMoney(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// MoneyPtr is a convenience for optional amounts.
func MoneyPtr(m Money) *Money { return &m }

// Rate is a tax rate in basis points (1/100 of a percent): 19% = 1900.
// Stored as an integer for the same reason as Money.
type Rate int64

// ParseRatePercent parses the percentage printed on a receipt ("19", "7.7", "5,5").
func ParseRatePercent(s string) (Rate, error) {
	m, err := ParseMoney(s) // same shape: up to two decimals
	if err != nil {
		return 0, fmt.Errorf("invalid rate %q", s)
	}
	return Rate(m), nil
}

// String renders the rate as a fraction: 1900 -> "0.19", 770 -> "0.077".
func (r Rate) String() string {
	s := fmt.Sprintf("%d.%04d", int64(r)/10000, int64(r)%10000)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// MarshalJSON emits the rate as a fraction number (0.19), matching gold.json.
func (r Rate) MarshalJSON() ([]byte, error) { return []byte(r.String()), nil }
