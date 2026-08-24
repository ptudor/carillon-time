package refclock

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func sentence(body string) string {
	var sum byte
	for i := range body {
		sum ^= body[i]
	}
	return fmt.Sprintf("$%s*%02X", body, sum)
}

func TestParseNMEARMC(t *testing.T) {
	s, err := parseNMEA(sentence("GNRMC,123519.00,A,4807.038,N,01131.000,E,022.4,084.4,230826,003.1,W"))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 23, 12, 35, 19, 0, time.UTC)
	if s.Kind != "RMC" || !s.Valid || s.Fractional || !s.Timestamp.Equal(want) {
		t.Fatalf("RMC: %+v", s)
	}

	invalid, err := parseNMEA(sentence("GPRMC,123519,V,,,,,,,230826,,,"))
	if err != nil || invalid.Valid {
		t.Fatalf("invalid-fix RMC: %+v %v", invalid, err)
	}
}

func TestParseNMEAZDAAndGGA(t *testing.T) {
	zda, err := parseNMEA(sentence("GPZDA,201530.00,04,07,2026,00,00"))
	if err != nil || zda.Kind != "ZDA" || !zda.Valid || !zda.Timestamp.Equal(time.Date(2026, 7, 4, 20, 15, 30, 0, time.UTC)) {
		t.Fatalf("ZDA: %+v %v", zda, err)
	}
	gga, err := parseNMEA(sentence("GPGGA,123519,4807.038,N,01131.000,E,2,08,0.9,545.4,M,46.9,M,,"))
	if err != nil || gga.Kind != "GGA" || !gga.Valid || gga.FixQuality != 2 || gga.Satellites != 8 {
		t.Fatalf("GGA: %+v %v", gga, err)
	}
}

func TestParseNMEARejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"missing checksum", "$GPRMC,123519,A", "checksum"},
		{"bad checksum", "$GPRMC,123519,A*00", "mismatch"},
		{"bad talker", sentence("XXRMC,123519,A,,,,,,,230826,,,"), "talker"},
		{"unsupported", sentence("GPTXT,01,01,01,text"), "unsupported"},
		{"truncated RMC", sentence("GPRMC,123519,A"), "truncated"},
		{"bad date", sentence("GPRMC,123519,A,,,,,,,310226,,,"), "calendar"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseNMEA(tt.line)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want %q", err, tt.want)
			}
		})
	}
}

func TestParseNMEAMarksNonzeroFraction(t *testing.T) {
	s, err := parseNMEA(sentence("GARMC,123519.25,A,,,,,,,230826,,,"))
	if err != nil || !s.Fractional || s.Timestamp.Nanosecond() != 250_000_000 {
		t.Fatalf("fractional RMC: %+v %v", s, err)
	}
	zero, err := parseNMEA(sentence("BDRMC,123519.000,A,,,,,,,230826,,,"))
	if err != nil || zero.Fractional {
		t.Fatalf("zero-fraction RMC: %+v %v", zero, err)
	}
}
