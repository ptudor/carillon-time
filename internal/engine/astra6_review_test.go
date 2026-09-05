package engine

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"carillon/internal/clock"
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

// TestAstra6SweepPreservesConfiguredDrift is the review's RA6X-015 probe. The
// drift filename is operator-configurable, so it can legitimately look like a
// temporary; the sweep used to glob `.drift-*`, stat it, and delete anything
// older than a minute, taking the host's calibration with it.
func TestAstra6SweepPreservesConfiguredDrift(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, ".drift-calibrated")
	if err := os.WriteFile(drift, []byte("17.382812\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(drift, old, old); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewFake(time.Now())
	if _, err := New(testConfig(drift), clk, quietLog()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(drift)
	if err != nil {
		t.Fatalf("the configured drift file was deleted: %v", err)
	}
	if strings.TrimSpace(string(b)) != "17.382812" {
		t.Fatalf("drift file contents changed: %q", b)
	}
}

// TestAstra6SweepScope covers RA6X-015's verification list: matching and
// non-matching names, symlinks, directories, the destination itself, and
// another instance's temporaries. Only this writer's abandoned temporaries
// may disappear.
func TestAstra6SweepScope(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, "drift")
	if err := os.WriteFile(drift, []byte("3.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)

	plantFile := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plantDir := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Removed: this writer's own abandoned temporaries, in both the current
	// and the legacy naming scheme (the destination is the default "drift").
	goes := []string{
		plantFile(".drift-tmp-123456"),
		plantFile(".drift-654321"),
	}
	// Kept.
	stays := []string{
		plantFile(".drift-tmp-notdigits"),  // not a name CreateTemp makes
		plantFile(".drift-calibrated"),     // a plausible operator filename
		plantFile(".driftB-tmp-123456"),    // another instance's temporary
		plantFile(".drift-tmp-123456.bak"), // suffixed, not ours
		plantFile("drift-tmp-123456"),      // not hidden, not ours
		plantFile(".unrelated-999"),        // nothing to do with carillon
		plantDir(".drift-tmp-777777"),      // a directory, never removed
	}
	// A symlink named like a temporary must not be followed or removed.
	link := filepath.Join(dir, ".drift-tmp-888888")
	target := plantFile("precious")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A fresh temporary of ours: another instance may still be writing it.
	fresh := filepath.Join(dir, ".drift-tmp-111111")
	if err := os.WriteFile(fresh, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	stays = append(stays, link, target, fresh, drift)

	clk := clock.NewFake(time.Now())
	if _, err := New(testConfig(drift), clk, quietLog()); err != nil {
		t.Fatal(err)
	}
	for _, p := range goes {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("abandoned temporary %s survived: %v", filepath.Base(p), err)
		}
	}
	for _, p := range stays {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s must not have been removed: %v", filepath.Base(p), err)
		}
	}
}

// TestAstra6SweepIsInstanceScoped checks that two carillon instances sharing
// a state directory cannot delete each other's abandoned temporaries.
func TestAstra6SweepIsInstanceScoped(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "carillon.drift")
	theirs := filepath.Join(dir, "other.drift")
	for _, p := range []string{mine, theirs} {
		if err := os.WriteFile(p, []byte("1.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	plant := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ours := plant(".carillon.drift-tmp-100")
	other := plant(".other.drift-tmp-200")

	clk := clock.NewFake(time.Now())
	if _, err := New(testConfig(mine), clk, quietLog()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(ours); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("our own abandoned temporary survived: %v", err)
	}
	if _, err := os.Lstat(other); err != nil {
		t.Errorf("another instance's temporary was deleted: %v", err)
	}
}

// TestAstra6WriteDriftUsesItsOwnNamespace proves the temporaries writeDrift
// creates are exactly the ones the sweep recognises.
func TestAstra6WriteDriftUsesItsOwnNamespace(t *testing.T) {
	dir := t.TempDir()
	drift := filepath.Join(dir, "state.drift")
	if err := writeDrift(drift, 12.5); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.drift" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("writeDrift left %v behind", names)
	}
	if got := driftTempPattern(drift); got != ".state.drift-tmp-*" {
		t.Fatalf("temp pattern %q is not destination-specific", got)
	}
}
