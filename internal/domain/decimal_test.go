package domain

import (
	"encoding/json"
	"testing"
)

func TestParseDecimal(t *testing.T) {
	cases := []struct {
		in    string
		want  Decimal
		print string
	}{
		{"19", Decimal{19, 0}, "19"},
		{"19.123", Decimal{19123, 3}, "19.123"},
		{"7,7", Decimal{77, 1}, "7.7"},
		{"8.875", Decimal{8875, 3}, "8.875"},
		{"1.50", Decimal{15, 1}, "1.5"}, // normalized: no trailing zeros
		{"-0.25", Decimal{-25, 2}, "-0.25"},
		{".5", Decimal{5, 1}, "0.5"},
		{"0.000", Decimal{0, 0}, "0"},
		{"0.000000001", Decimal{1, 9}, "0.000000001"},
	}
	for _, c := range cases {
		got, err := ParseDecimal(c.in)
		if err != nil || got != c.want || got.String() != c.print {
			t.Errorf("ParseDecimal(%q) = %+v %q %v, want %+v %q", c.in, got, got.String(), err, c.want, c.print)
		}
	}
	for _, bad := range []string{"", ".", "1.", "1e3", "abc", "1.2.3", "0.0000000001", "1234567890123456789"} {
		if d, err := ParseDecimal(bad); err == nil {
			t.Errorf("ParseDecimal(%q) = %+v, want an error", bad, d)
		}
	}
}

// Rates keep every printed digit and still render as the fraction gold.json uses.
func TestRatePrecision(t *testing.T) {
	cases := map[string]string{"19": "0.19", "7.7": "0.077", "19.123": "0.19123", "8.875": "0.08875", "0": "0", "100": "1"}
	for in, want := range cases {
		r, err := ParseRatePercent(in)
		if err != nil || r.String() != want {
			t.Errorf("rate %s%% -> %q %v, want %q", in, r.String(), err, want)
		}
	}
	for _, bad := range []string{"-1", "1.12345678"} {
		if _, err := ParseRatePercent(bad); err == nil {
			t.Errorf("rate %q accepted", bad)
		}
	}
}

func TestDecimalJSON(t *testing.T) {
	var v struct{ Q *Decimal }
	if err := json.Unmarshal([]byte(`{"Q": 1.125}`), &v); err != nil || *v.Q != (Decimal{1125, 3}) {
		t.Fatalf("number: %+v %v", v.Q, err)
	}
	if err := json.Unmarshal([]byte(`{"Q": "2.50"}`), &v); err != nil || *v.Q != (Decimal{25, 1}) {
		t.Fatalf("string: %+v %v", v.Q, err)
	}
	if b, _ := json.Marshal(Decimal{1125, 3}); string(b) != "1.125" {
		t.Fatalf("marshal: %s", b)
	}
}

// Whatever parses must print back to the same value.
func FuzzParseDecimal(f *testing.F) {
	for _, s := range []string{"19.123", "-0.5", "1,5", "0.000000001", "999999999999999999"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecimal(s)
		if err != nil {
			return
		}
		back, err := ParseDecimal(d.String())
		if err != nil || back != d {
			t.Fatalf("%q -> %+v -> %q -> %+v %v", s, d, d.String(), back, err)
		}
	})
}
