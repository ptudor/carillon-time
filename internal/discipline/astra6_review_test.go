package discipline

import (
	"fmt"
	"testing"
)

// TestAstra6NeverStepIncludesPanicStartup is the review's RA6X-011 probe.
// panic_at_startup used to reach the step directly, so an explicit
// limit = 0 ("never step") could still move the clock by 2000 seconds.
func TestAstra6NeverStepIncludesPanicStartup(t *testing.T) {
	cfg := loopCfg()
	cfg.StepLimit = 0
	cfg.PanicAtStartup = true
	u := NewLoop(cfg, 0, true).Update(2000, 6, 1, false, true)
	if u.Stepped {
		t.Fatal("limit=0 and panic_at_startup=true issued a 2000-second step")
	}
	if !u.PanicRefused {
		t.Fatal("a correction that cannot be stepped must be refused, not slewed")
	}
	if len(u.Actions) != 0 {
		t.Fatalf("refused panic correction produced actions: %+v", u.Actions)
	}
}

// TestAstra6StepPolicyCrossProduct is RA6X-011's verification table: the
// cross product of limit 0/positive/-1, panic_at_startup false/true, first
// and later update, and both signs of offset around both thresholds.
func TestAstra6StepPolicyCrossProduct(t *testing.T) {
	const (
		threshold = 0.5
		panicAt   = 1000.0
	)
	// Offsets either side of each threshold, both signs.
	offsets := []struct {
		name   string
		value  float64
		beyond string // "small", "step", "panic"
	}{
		{"tiny +", 0.1, "small"},
		{"tiny -", -0.1, "small"},
		{"past threshold +", 10, "step"},
		{"past threshold -", -10, "step"},
		{"just past threshold +", threshold + 1e-9, "step"},
		{"just under threshold +", threshold - 1e-9, "small"},
		{"past panic +", 2000, "panic"},
		{"past panic -", -2000, "panic"},
		{"just past panic +", panicAt + 1, "panic"},
		{"just under panic +", panicAt - 1, "step"},
	}
	for _, limit := range []int{0, 3, -1} {
		for _, atStartup := range []bool{false, true} {
			for _, first := range []bool{true, false} {
				for _, o := range offsets {
					name := label(limit, atStartup, first, o.name)
					t.Run(name, func(t *testing.T) {
						cfg := loopCfg()
						cfg.StepThreshold = threshold
						cfg.Panic = panicAt
						cfg.StepLimit = limit
						cfg.PanicAtStartup = atStartup
						l := NewLoop(cfg, 0, true)
						now := 1.0
						if !first {
							// One ordinary in-range update first, so
							// Updates > 0 without consuming a step.
							l.Update(0.001, 6, now, false, true)
							now++
						}
						u := l.Update(o.value, 6, now, false, true)

						stepsAllowed := limit < 0 || (limit > 0 && (first || l.Updates-1 < limit))
						switch o.beyond {
						case "panic":
							// The startup exception applies only on the
							// first update, and only when stepping is
							// permitted at all.
							want := first && atStartup && stepsAllowed
							if u.Stepped != want {
								t.Fatalf("stepped=%v, want %v", u.Stepped, want)
							}
							if !want && !u.PanicRefused {
								t.Fatal("panic offset neither stepped nor refused")
							}
							if want && u.PanicRefused {
								t.Fatal("a permitted startup correction was also refused")
							}
						case "step":
							if u.PanicRefused {
								t.Fatal("an offset below panic was panic-refused")
							}
							if u.Stepped != stepsAllowed {
								t.Fatalf("stepped=%v, want %v", u.Stepped, stepsAllowed)
							}
						case "small":
							if u.Stepped || u.PanicRefused {
								t.Fatalf("small offset produced stepped=%v refused=%v", u.Stepped, u.PanicRefused)
							}
						}
						if u.Stepped {
							if len(u.Actions) == 0 || u.Actions[0].Kind != ActionStep {
								t.Fatalf("Stepped without an ActionStep: %+v", u.Actions)
							}
							if u.Actions[0].Value != o.value {
								t.Fatalf("stepped %v, want %v", u.Actions[0].Value, o.value)
							}
						} else {
							for _, a := range u.Actions {
								if a.Kind == ActionStep {
									t.Fatalf("ActionStep without Stepped: %+v", u.Actions)
								}
							}
						}
					})
				}
			}
		}
	}
}

func label(limit int, atStartup, first bool, offset string) string {
	return fmt.Sprintf("limit=%d/panic_at_startup=%v/first=%v/%s", limit, atStartup, first, offset)
}
