package engine

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestAstra6DriftRejectsNaN is the review's RA6X-014 probe. strconv accepts
// "NaN", and every subsequent range comparison is false against it, so the
// value used to be marked a known frequency and handed to the loop.
func TestAstra6DriftRejectsNaN(t *testing.T) {
	p := filepath.Join(t.TempDir(), "drift")
	if err := os.WriteFile(p, []byte("NaN\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if v, err := readDrift(p); err == nil {
		t.Fatalf("non-finite drift accepted: %v", v)
	}
}

// TestAstra6DriftFileContents covers RA6X-014's verification list: every NaN
// and infinity spelling strconv accepts, out-of-range values, whitespace,
// the ±500 ppm boundaries, and ordinary round trips.
func TestAstra6DriftFileContents(t *testing.T) {
	cases := []struct {
		content string
		want    float64
		ok      bool
	}{
		// Non-finite: strconv parses all of these.
		{"NaN\n", 0, false},
		{"nan\n", 0, false},
		{"-nan\n", 0, false},
		{"NAN", 0, false},
		{"Inf\n", 0, false},
		{"inf", 0, false},
		{"+Inf\n", 0, false},
		{"-Inf\n", 0, false},
		{"Infinity\n", 0, false},
		{"-infinity", 0, false},
		// Out of the ±500 ppm range the kernel accepts.
		{"500.000001\n", 0, false},
		{"-500.000001\n", 0, false},
		{"1e6\n", 0, false},
		// Not a number at all.
		{"", 0, false},
		{"   \n", 0, false},
		{"12.5 ppm\n", 0, false},
		// Go's ParseFloat also accepts hexadecimal floats. 0x1p3 is a
		// finite, in-range 8 ppm: unusual to find in a drift file, but not
		// a value the daemon has any reason to refuse.
		{"0x1p3\n", 8, true},
		// Valid, including the boundaries and surrounding whitespace.
		{"0\n", 0, true},
		{"12.5\n", 12.5, true},
		{"-12.5\n", -12.5, true},
		{"  \t 17.382812\r\n", 17.382812, true},
		{"500\n", 500, true},
		{"-500\n", -500, true},
		{"500.0000000\n", 500, true},
		{"1e-6", 1e-6, true},
	}
	for _, c := range cases {
		t.Run(c.content, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "drift")
			if err := os.WriteFile(p, []byte(c.content), 0600); err != nil {
				t.Fatal(err)
			}
			v, err := readDrift(p)
			switch {
			case c.ok && err != nil:
				t.Fatalf("readDrift(%q) = %v, want %v", c.content, err, c.want)
			case !c.ok && err == nil:
				t.Fatalf("readDrift(%q) accepted %v", c.content, v)
			case c.ok && v != c.want:
				t.Fatalf("readDrift(%q) = %v, want %v", c.content, v, c.want)
			}
		})
	}
}

// TestAstra6DriftRoundTrip proves a written value reads back unchanged, so
// the stricter parser did not narrow what writeDrift can produce.
func TestAstra6DriftRoundTrip(t *testing.T) {
	for _, ppm := range []float64{0, 12.5, -12.5, 17.382812, 499.999999, -499.999999, 500, -500} {
		p := filepath.Join(t.TempDir(), "drift")
		if err := writeDrift(p, ppm); err != nil {
			t.Fatalf("writeDrift(%v): %v", ppm, err)
		}
		got, err := readDrift(p)
		if err != nil {
			t.Fatalf("readDrift after writeDrift(%v): %v", ppm, err)
		}
		if math.Abs(got-ppm) > 1e-6 {
			t.Fatalf("round trip of %v gave %v", ppm, got)
		}
	}
}

// TestAstra6NonFiniteNeverReachesTheActuator checks the second half of
// RA6X-014's fix: even if an invalid frequency somehow appeared inside the
// daemon, the actuator boundary refuses it rather than converting it to an
// arbitrary kernel word.
func TestAstra6NonFiniteNeverReachesTheActuator(t *testing.T) {
	for _, ppm := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		clk, e := newBoundsEngine(t)
		err := e.handle(actions(actionSetFrequency(ppm)), 1)
		if err == nil {
			t.Fatalf("SetFrequency(%v) accepted", ppm)
		}
		if len(clk.Frequencies) != 0 {
			t.Fatalf("SetFrequency(%v) reached the actuator: %v", ppm, clk.Frequencies)
		}
	}
}
