package refclock

import (
	"math"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/clock"
	"github.com/ptudor/carillon-time/internal/ntp"
)

func testNMEA(t *testing.T) (*NMEA, *clock.Fake, *PulseTracker) {
	t.Helper()
	start := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	pulse := &PulseTracker{}
	cfg := NMEAConfig{
		Name: "gps/nmea", Device: "/dev/ttyS0", Baud: 9600, Offset: 0.15,
		Sentences: []string{"RMC", "ZDA"}, Pulse: pulse,
	}
	return newNMEA(cfg, clk, nil, nil, nil), clk, pulse
}

func TestNMEAProducesRobustMeasurementAndStatus(t *testing.T) {
	n, clk, pulse := testNMEA(t)
	base := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var lastValid bool
	for i := 0; i < 4; i++ {
		stamp := base.Add(time.Duration(i) * time.Second)
		arrival := stamp.Add(150*time.Millisecond + time.Duration(i%2)*time.Millisecond)
		pulse.Observe(stamp)
		m, ok := n.acceptLine(sentence("GPRMC,"+stamp.Format("150405")+",A,,,,,,,"+stamp.Format("020106")+",,,"), arrival, float64(i))
		if !ok {
			t.Fatalf("sentence %d was not accepted", i)
		}
		lastValid = m.Valid
		if i < 3 && m.Valid {
			t.Fatalf("measurement became valid with only %d samples", i+1)
		}
		if m.RefID != ntp.RefIDFromString("GPS") || m.SourceRefID != m.RefID || m.Stratum != 0 {
			t.Fatalf("identity: %+v", m)
		}
		clk.Advance(time.Second)
	}
	if !lastValid {
		t.Fatal("four-sample NMEA window did not become valid")
	}
	info := n.Info()
	if info.Reach != 0x0f || info.Received != 4 || !info.Refclock.Stable || !info.Refclock.FixValid {
		t.Fatalf("info: %+v refclock=%+v", info, info.Refclock)
	}
	if math.Abs(info.Offset+0.0005) > 0.001 || math.Abs(info.Refclock.MeasuredLag-0.1505) > 0.001 {
		t.Fatalf("offset/lag: offset=%g lag=%g", info.Offset, info.Refclock.MeasuredLag)
	}
}

func TestNMEAFramingAnchorsAtDollarRead(t *testing.T) {
	n, _, _ := testNMEA(t)
	stamp := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	line := sentence("GNZDA,120000.00,23,08,2026,00,00") + "\r\n"
	if got := n.consume([]byte("noise"+line[:8]), stamp.Add(100*time.Millisecond), 1, 0); len(got) != 0 {
		t.Fatalf("partial sentence emitted: %+v", got)
	}
	got := n.consume([]byte(line[8:]), stamp.Add(300*time.Millisecond), 1.2, 0)
	if len(got) != 1 || math.Abs(got[0].Offset-0.05) > 1e-12 {
		t.Fatalf("framed measurement: %+v", got)
	}
}

func TestNMEAIgnoresFractionAndSuspectedRollover(t *testing.T) {
	n, _, _ := testNMEA(t)
	n.cfg.BuildTime = time.Date(2026, 8, 23, 18, 0, 0, 0, time.UTC)
	arrival := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if _, ok := n.acceptLine(sentence("GPRMC,120000.25,A,,,,,,,230826,,,"), arrival, 1); ok {
		t.Fatal("fractional sentence accepted")
	}
	if _, ok := n.acceptLine(sentence("GPRMC,120000,A,,,,,,,220826,,,"), arrival, 2); ok {
		t.Fatal("pre-build date accepted")
	}
	if info := n.Info(); info.Bogus != 2 {
		t.Fatalf("rejections not counted: %+v", info)
	}
}

func TestNMEATimeoutShiftsReach(t *testing.T) {
	n, _, _ := testNMEA(t)
	stamp := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if _, ok := n.acceptLine(sentence("GPRMC,120000,A,,,,,,,230826,,,"), stamp.Add(150*time.Millisecond), 1); !ok {
		t.Fatal("initial sentence rejected")
	}
	m, ok := n.tick(3.6)
	if !ok || m.Reach != 0x04 || n.Info().Timeouts != 2 {
		t.Fatalf("timeout: ok=%v measurement=%+v info=%+v", ok, m, n.Info())
	}
}
