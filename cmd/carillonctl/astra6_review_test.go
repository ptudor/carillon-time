package main

import "testing"

// TestAstra6WaitSyncTimeoutValidation covers the CLI half of RA6X-035.
// strconv.ParseFloat accepts "NaN" and "Inf", and the old `secs < 0` test let
// both through to a conversion that overflows time.Duration. Zero keeps its
// documented meaning of waiting for ever.
func TestAstra6WaitSyncTimeoutValidation(t *testing.T) {
	cases := []struct {
		arg  string
		want float64
		ok   bool
	}{
		{"NaN", 0, false},
		{"nan", 0, false},
		{"Inf", 0, false},
		{"+Inf", 0, false},
		{"-Inf", 0, false},
		{"-1", 0, false},
		{"1e30", 0, false},
		{"1e10", 0, false},  // past the representable limit
		{"1e-12", 0, false}, // rounds to zero, which would silently mean "for ever"
		{"not-a-number", 0, false},
		{"", 0, false},
		{"0", 0, true},
		{"1", 1, true},
		{"0.5", 0.5, true},
		{"60", 60, true},
		{"1e-9", 1e-9, true},
	}
	for _, c := range cases {
		t.Run(c.arg, func(t *testing.T) {
			got, err := parseWaitTimeout(c.arg)
			switch {
			case c.ok && err != nil:
				t.Fatalf("timeout %q refused: %v", c.arg, err)
			case !c.ok && err == nil:
				t.Fatalf("timeout %q accepted as %v", c.arg, got)
			case c.ok && got != c.want:
				t.Fatalf("timeout %q = %v, want %v", c.arg, got, c.want)
			}
		})
	}
}
