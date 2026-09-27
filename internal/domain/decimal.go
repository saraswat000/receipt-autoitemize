package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// MaxDecimalScale bounds the digits after the point (nanounits). Enough for any tax
// rate or quantity on a receipt, and it keeps Value × 10^Scale well inside int64.
const MaxDecimalScale = 9

// Decimal is an exact decimal number: Value × 10^-Scale, so 19.123 is {19123, 3}.
// It is stored as two INTEGER columns (value, scale), which keeps any precision the
// receipt prints without a float ever touching it. Values are normalized (no
// trailing fractional zeros), so equal numbers have equal representations.
type Decimal struct {
	Value int64
	Scale uint8
}

// ParseDecimal parses "19", "19.123", "7,7" or "-1.5". Exponents ("1e3") and more
// than MaxDecimalScale fractional digits are rejected rather than rounded.
func ParseDecimal(s string) (Decimal, error) {
	s = strings.TrimSpace(strings.Replace(s, ",", ".", 1))
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if s == "" || (hasFrac && frac == "") || len(frac) > MaxDecimalScale || !isDigits(whole) || !isDigits(frac) {
		return Decimal{}, fmt.Errorf("invalid decimal %q", s)
	}
	digits := strings.TrimLeft(whole+frac, "0")
	if len(digits) > 18 { // 10^18 < 2^63
		return Decimal{}, fmt.Errorf("decimal %q has too many digits", s)
	}
	v, _ := strconv.ParseInt("0"+digits, 10, 64)
	if neg {
		v = -v
	}
	return Decimal{Value: v, Scale: uint8(len(frac))}.normalize(), nil
}

// NewDecimal builds a normalized Decimal; it panics on a scale beyond the maximum,
// which is a programming error, not input.
func NewDecimal(value int64, scale uint8) Decimal {
	if scale > MaxDecimalScale {
		panic(fmt.Sprintf("decimal scale %d > %d", scale, MaxDecimalScale))
	}
	return Decimal{value, scale}.normalize()
}

func (d Decimal) normalize() Decimal {
	for d.Scale > 0 && d.Value%10 == 0 {
		d.Value /= 10
		d.Scale--
	}
	if d.Value == 0 {
		d.Scale = 0
	}
	return d
}

// Sign returns -1, 0 or 1.
func (d Decimal) Sign() int {
	switch {
	case d.Value < 0:
		return -1
	case d.Value > 0:
		return 1
	}
	return 0
}

// String renders the exact value: {19123, 3} -> "19.123".
func (d Decimal) String() string { return formatScaled(d.Value, int(d.Scale)) }

func formatScaled(v int64, scale int) string {
	sign := ""
	u := uint64(v)
	if v < 0 {
		sign, u = "-", uint64(-v)
	}
	s := strconv.FormatUint(u, 10)
	if scale == 0 {
		return sign + s
	}
	if len(s) <= scale {
		s = strings.Repeat("0", scale-len(s)+1) + s
	}
	return sign + s[:len(s)-scale] + "." + s[len(s)-scale:]
}

// MarshalJSON emits a JSON number with the exact digits.
func (d Decimal) MarshalJSON() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalJSON accepts a JSON number or a numeric string, parsed exactly.
func (d *Decimal) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		b = []byte(s)
	}
	v, err := ParseDecimal(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Rate is a tax rate held as an exact percentage: 19% = {19, 0}, 19.123% =
// {19123, 3}. It is stored as rate_value / rate_scale.
type Rate struct{ Percent Decimal }

// ParseRatePercent parses the percentage printed on a receipt ("19", "7.7", "8.875").
func ParseRatePercent(s string) (Rate, error) {
	d, err := ParseDecimal(s)
	if err != nil || d.Sign() < 0 || d.Scale > MaxDecimalScale-2 {
		return Rate{}, fmt.Errorf("invalid rate %q", s)
	}
	return Rate{d}, nil
}

// String renders the rate as a fraction, matching gold.json: 19% -> "0.19",
// 7.7% -> "0.077", 19.123% -> "0.19123".
func (r Rate) String() string {
	return NewDecimal(r.Percent.Value, r.Percent.Scale+2).String()
}

// MarshalJSON emits the rate as a fraction number (0.19).
func (r Rate) MarshalJSON() ([]byte, error) { return []byte(r.String()), nil }
