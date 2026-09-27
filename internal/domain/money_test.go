package domain

import (
	"encoding/json"
	"testing"
)

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in   string
		want Money
		ok   bool
	}{
		{"3.50", 350, true},
		{"3,50", 350, true},
		{"17.85", 1785, true},
		{"24", 2400, true},
		{"0.5", 50, true},
		{".5", 50, true},
		{"-1.90", -190, true},
		{"+2", 200, true},
		{"1.999", 0, false}, // never silently round
		{"", 0, false},
		{"1.", 0, false},
		{"--1", 0, false},
		{"abc", 0, false},
		{"1e3", 0, false},
	}
	for _, c := range cases {
		got, err := ParseMoney(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("ParseMoney(%q) = %v, %v; want %v ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}

func TestMoneyJSON(t *testing.T) {
	b, _ := json.Marshal(struct {
		A Money `json:"a"`
		B Money `json:"b"`
	}{1785, -5})
	if string(b) != `{"a":17.85,"b":-0.05}` {
		t.Fatalf("got %s", b)
	}
	var m struct{ A, B Money }
	if err := json.Unmarshal([]byte(`{"A": 4.4, "B": "8.90"}`), &m); err != nil || m.A != 440 || m.B != 890 {
		t.Fatalf("unmarshal: %+v %v", m, err)
	}
	if err := json.Unmarshal([]byte(`{"A": 0.001}`), &m); err == nil {
		t.Fatal("expected error for sub-cent amount")
	}
}

func TestRate(t *testing.T) {
	for in, want := range map[string]string{"19": "0.19", "7.7": "0.077", "5,5": "0.055", "0": "0", "100": "1"} {
		r, err := ParseRatePercent(in)
		if err != nil || r.String() != want {
			t.Errorf("rate %q -> %q, %v; want %q", in, r.String(), err, want)
		}
	}
}

// Any amount ParseMoney accepts must print back to a value that parses to the same
// cents, and nothing may overflow into a wrong sign.
func FuzzParseMoney(f *testing.F) {
	for _, s := range []string{"0", "3.50", "-1,9", "24", "99999999999999999999", "+.5", "1e3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseMoney(s)
		if err != nil {
			return
		}
		back, err := ParseMoney(m.String())
		if err != nil || back != m {
			t.Fatalf("%q -> %d -> %q -> %d %v", s, m, m.String(), back, err)
		}
	})
}
