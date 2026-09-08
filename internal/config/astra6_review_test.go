package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// astra6Valid returns a configuration Validate accepts, so a probe can mutate
// exactly one field and attribute the resulting error to it.
func astra6Valid() *Config {
	c := Default()
	c.Leap.Acquire = "nist"
	c.Servers = []Server{{Name: "a", Address: "192.0.2.1", PollMin: 6, PollMax: 10}}
	c.Serve.Allow = []string{"127.0.0.0/8"}
	c.Serve.Listen = []string{"127.0.0.1:123"}
	return c
}

// astra6Refclock parses a refclock block through Parse so it carries the same
// defaults an operator's file would, then validates the whole configuration.
func astra6Refclock(t *testing.T, block string) error {
	t.Helper()
	doc := "[leap]\nacquire = \"nist\"\n[[server]]\naddress = \"192.0.2.1\"\n\n[serve]\nlisten = [\"127.0.0.1:123\"]\nallow = [\"127.0.0.0/8\"]\n\n" + block
	cfg, err := Parse([]byte(doc))
	if err != nil {
		return err
	}
	return Validate(cfg)
}

// TestAstra6RefclockOffsetsMustBePlausible covers the configuration half of
// RA6X-041: a finite calibration offset can still be enormous, and an
// enormous one reaches the measurement path where float-seconds arithmetic
// saturates. These settings compensate cable, driver and sentence lag, all
// far below a second.
func TestAstra6RefclockOffsetsMustBePlausible(t *testing.T) {
	cases := []struct {
		offset string
		ok     bool
	}{
		{"nan", false},
		{"inf", false},
		{"+inf", false},
		{"-inf", false},
		{"1e30", false},
		{"-1e30", false},
		{"1.0000000000000002", false}, // the next float above the limit
		{"2.0", false},
		{"1.0", true},
		{"-1.0", true},
		{"0.00000025", true},
		{"0.0", true},
		{"-0.15", true},
	}
	for _, c := range cases {
		t.Run(c.offset, func(t *testing.T) {
			err := astra6Refclock(t, fmt.Sprintf(
				"[[refclock]]\ntype = \"pps\"\ndevice = \"/dev/pps0\"\noffset = %s\n", c.offset))
			if c.ok && err != nil {
				t.Fatalf("valid offset %s refused: %v", c.offset, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("implausible offset %s accepted", c.offset)
			}
		})
	}
}

// TestAstra6GPSOffsetsMustBePlausible is the same check for the two GPS
// calibration keys.
func TestAstra6GPSOffsetsMustBePlausible(t *testing.T) {
	cases := []struct {
		name, pps, nmea string
		ok              bool
	}{
		{"pps huge", "1e30", "0.150", false},
		{"pps -inf", "-inf", "0.150", false},
		{"pps nan", "nan", "0.150", false},
		{"nmea huge", "0.0", "1e30", false},
		{"nmea nan", "0.0", "nan", false},
		{"nmea just over", "0.0", "1.0000000000000002", false},
		{"typical", "0.0", "0.150", true},
		{"nmea one second", "0.0", "1.0", true},
		{"nmea negative", "0.0", "-0.05", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := astra6Refclock(t, fmt.Sprintf(
				"[[refclock]]\ntype = \"gps\"\ndevice = \"/dev/ttyS0\"\npps = \"dcd\"\npps_offset = %s\nnmea_offset = %s\n",
				c.pps, c.nmea))
			if c.ok && err != nil {
				t.Fatalf("valid offsets refused: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("implausible offset accepted")
			}
		})
	}
}

// TestAstra6ShippedExamplesStillParse guards the calibration bound against
// rejecting anything the project ships: the in-test colo document and the
// commented example operators start from.
func TestAstra6ShippedExamplesStillParse(t *testing.T) {
	docs := map[string][]byte{"colo": []byte(coloExample)}
	shipped, err := os.ReadFile(filepath.Join("..", "..", "deploy", "carillon.toml.example"))
	if err != nil {
		t.Fatalf("reading the shipped example: %v", err)
	}
	docs["deploy/carillon.toml.example"] = shipped
	for name, body := range docs {
		cfg, err := Parse(body)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}
		if err := Validate(cfg); err != nil {
			t.Fatalf("%s: validate: %v", name, err)
		}
	}
}

// TestAstra6ConfigurationRejectsInfinity is the review's RA6X-035 probe.
// Several checks were one-sided comparisons such as `!(v > 0)`, which reject
// a NaN but accept +Inf.
func TestAstra6ConfigurationRejectsInfinity(t *testing.T) {
	mutations := map[string]func(*Config){
		"drift_seconds": func(c *Config) { c.Daemon.DriftStableSeconds = math.Inf(1) },
		"drift_spread":  func(c *Config) { c.Daemon.DriftStableSpreadPPM = math.Inf(1) },
		"holdover":      func(c *Config) { c.Discipline.HoldoverMax = math.Inf(1) },
		"panic":         func(c *Config) { c.Step.Panic = math.Inf(1) },
		"rate":          func(c *Config) { c.Serve.RateLimitPPS = math.Inf(1) },
		"burst":         func(c *Config) { c.Serve.RateBurst = math.Inf(1) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := astra6Valid()
			if err := Validate(c); err != nil {
				t.Fatalf("setup: %v", err)
			}
			mutate(c)
			if err := Validate(c); err == nil {
				t.Fatal("infinite setting accepted")
			}
		})
	}
}

// TestAstra6NumericSettingBounds is RA6X-035's table: NaN, both infinities,
// the maximum representable number of seconds, values that just overflow,
// sub-nanosecond values that would round to zero, and the ordinary boundary
// settings that must keep working.
func TestAstra6NumericSettingBounds(t *testing.T) {
	// Every field that names seconds shares the same rules.
	secondsFields := map[string]func(*Config, float64){
		"drift_stable_seconds": func(c *Config, v float64) { c.Daemon.DriftStableSeconds = v },
		"holdover_max":         func(c *Config, v float64) { c.Discipline.HoldoverMax = v },
		"step.threshold":       func(c *Config, v float64) { c.Step.Threshold = v },
	}
	secondsCases := []struct {
		name  string
		value float64
		ok    bool
	}{
		{"nan", math.NaN(), false},
		{"+inf", math.Inf(1), false},
		{"-inf", math.Inf(-1), false},
		{"zero", 0, false},
		{"negative", -1, false},
		{"overflows duration", 1e30, false},
		{"just past the limit", 1e10, false},
		{"sub-nanosecond", 1e-12, false},
		{"one nanosecond", 1e-9, true},
		{"one second", 1, true},
		{"an hour", 3600, true},
	}
	for field, set := range secondsFields {
		for _, c := range secondsCases {
			t.Run(field+"/"+c.name, func(t *testing.T) {
				cfg := astra6Valid()
				set(cfg, c.value)
				// step.panic must stay above step.threshold.
				if field == "step.threshold" && c.ok {
					cfg.Step.Panic = c.value * 2
				}
				err := Validate(cfg)
				if c.ok && err != nil {
					t.Fatalf("%v refused: %v", c.value, err)
				}
				if !c.ok && err == nil {
					t.Fatalf("%v accepted", c.value)
				}
			})
		}
	}

	rangeFields := map[string]struct {
		set     func(*Config, float64)
		lo, hi  float64
		belowLo float64
	}{
		"drift_stable_spread_ppm": {func(c *Config, v float64) { c.Daemon.DriftStableSpreadPPM = v }, 1e-9, 1000, 0},
		"rate_limit_pps":          {func(c *Config, v float64) { c.Serve.RateLimitPPS = v }, 1e-9, 1e9, 0},
		"rate_burst":              {func(c *Config, v float64) { c.Serve.RateBurst = v }, 1, 1e9, 0.5},
	}
	for field, spec := range rangeFields {
		cases := []struct {
			name  string
			value float64
			ok    bool
		}{
			{"nan", math.NaN(), false},
			{"+inf", math.Inf(1), false},
			{"-inf", math.Inf(-1), false},
			{"below the minimum", spec.belowLo, false},
			{"above the maximum", math.Nextafter(spec.hi, math.Inf(1)), false},
			{"far above the maximum", spec.hi * 1e6, false},
			{"the minimum", spec.lo, true},
			{"the maximum", spec.hi, true},
		}
		for _, c := range cases {
			t.Run(field+"/"+c.name, func(t *testing.T) {
				cfg := astra6Valid()
				spec.set(cfg, c.value)
				err := Validate(cfg)
				if c.ok && err != nil {
					t.Fatalf("%v refused: %v", c.value, err)
				}
				if !c.ok && err == nil {
					t.Fatalf("%v accepted", c.value)
				}
			})
		}
	}
}

// TestAstra6PanicStaysAboveThreshold checks the panic bound did not lose the
// ordering rule it used to carry on its own.
func TestAstra6PanicStaysAboveThreshold(t *testing.T) {
	cfg := astra6Valid()
	cfg.Step.Threshold = 0.5
	cfg.Step.Panic = 0.25
	if err := Validate(cfg); err == nil {
		t.Fatal("panic below threshold accepted")
	}
	cfg.Step.Panic = 1000
	if err := Validate(cfg); err != nil {
		t.Fatalf("valid step policy refused: %v", err)
	}
}

// TestAstra6DeviceIdentityFollowsTheDevice covers RA6X-039. Identity used to
// be path spelling, so a udev alias for the same node compared unequal.
func TestAstra6DeviceIdentityFollowsTheDevice(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "gps-alias")
	if err := os.Symlink(os.DevNull, alias); err != nil {
		t.Skipf("cannot create a device alias here: %v", err)
	}
	direct, err := deviceIdentity(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	viaAlias, err := deviceIdentity(alias)
	if err != nil {
		t.Fatal(err)
	}
	if direct != viaAlias {
		t.Fatalf("%s and its alias have different identities: %q vs %q", os.DevNull, direct, viaAlias)
	}
	if !strings.HasPrefix(direct, "chardev:") {
		t.Fatalf("a character device was identified as %q, not by its device number", direct)
	}

	// A different device must not collide.
	other, err := deviceIdentity("/dev/zero")
	if err != nil {
		t.Skipf("/dev/zero unavailable: %v", err)
	}
	if other == direct {
		t.Fatal("two different devices share an identity")
	}
}

// TestAstra6RejectsSharedDevices checks two refclocks cannot claim one
// device, however it is spelled, while one refclock using a device for both
// roles stays legitimate.
func TestAstra6RejectsSharedDevices(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(dir, "gps-alias")
	if err := os.Symlink(os.DevNull, alias); err != nil {
		t.Skipf("cannot create a device alias here: %v", err)
	}

	t.Run("two refclocks, two names for one device", func(t *testing.T) {
		errs := checkDeviceOwnership([]Refclock{
			{Name: "gps0", Type: "gps", Device: os.DevNull, PPS: "none"},
			{Name: "pps0", Type: "pps", Device: alias},
		})
		if len(errs) == 0 {
			t.Fatal("two refclocks claimed one device under two names")
		}
	})

	t.Run("one refclock using a device for both roles", func(t *testing.T) {
		// The supported FreeBSD arrangement: one callout tty carries both
		// the NMEA stream and the PPS edge.
		errs := checkDeviceOwnership([]Refclock{
			{Name: "gps0", Type: "gps", Device: os.DevNull, PPS: alias},
		})
		if len(errs) != 0 {
			t.Fatalf("a refclock using one device for both roles was rejected: %v", errs)
		}
	})

	t.Run("distinct devices are fine", func(t *testing.T) {
		errs := checkDeviceOwnership([]Refclock{
			{Name: "gps0", Type: "gps", Device: os.DevNull, PPS: "none"},
			{Name: "pps0", Type: "pps", Device: "/dev/zero"},
		})
		if len(errs) != 0 {
			t.Fatalf("distinct devices were reported as shared: %v", errs)
		}
	})

	t.Run("keyword pps values are not devices", func(t *testing.T) {
		errs := checkDeviceOwnership([]Refclock{
			{Name: "a", Type: "gps", Device: os.DevNull, PPS: "dcd"},
			{Name: "b", Type: "gps", Device: "/dev/zero", PPS: "cts"},
		})
		if len(errs) != 0 {
			t.Fatalf("pps keywords were treated as device paths: %v", errs)
		}
	})
}
