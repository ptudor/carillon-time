package leap

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"carillon/internal/ntp"
)

func ntpStamp(t time.Time) uint64 { return uint64(t.Unix()) + ntpUnixEpoch }

func leapList(first, second time.Time, firstOffset, secondOffset int, expiry time.Time) string {
	return fmt.Sprintf("#$ %d\n#@ %d\n%d %d # first\n%d %d # second\n",
		ntpStamp(first), ntpStamp(expiry), ntpStamp(first), firstOffset, ntpStamp(second), secondOffset)
}

func TestParseAndIndicator(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	expiry := time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC)
	table, err := Parse(strings.NewReader(leapList(first, transition, 37, 38, expiry)))
	if err != nil {
		t.Fatal(err)
	}
	if !table.Expiry.Equal(expiry) || !table.Updated.Equal(first) || len(table.Transitions) != 1 {
		t.Fatalf("table: %+v", table)
	}
	if got := table.Indicator(transition.Add(-24 * time.Hour)); got != ntp.LeapInsert {
		t.Fatalf("indicator at start = %v", got)
	}
	if got := table.Indicator(transition.Add(-time.Nanosecond)); got != ntp.LeapInsert {
		t.Fatalf("indicator before transition = %v", got)
	}
	if got := table.Indicator(transition); got != ntp.LeapNone {
		t.Fatalf("indicator after transition = %v", got)
	}
	if !table.Crossed(transition.Add(-time.Second), transition) || table.Crossed(transition, transition.Add(time.Second)) {
		t.Fatal("transition crossing was not edge-triggered")
	}
	if next, ok := table.Next(first); !ok || !next.At.Equal(transition) {
		t.Fatalf("next: %+v %v", next, ok)
	}
}

func TestDeletion(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	expiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	table, err := Parse(strings.NewReader(leapList(first, transition, 38, 37, expiry)))
	if err != nil || table.Indicator(transition.Add(-time.Hour)) != ntp.LeapDelete {
		t.Fatalf("deletion: %+v %v", table, err)
	}
}

func TestParseRejectsMalformedLists(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	transition := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	expiry := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		text string
		want string
	}{
		{"missing expiry", fmt.Sprintf("%d 37\n%d 38\n", ntpStamp(first), ntpStamp(transition)), "expiry"},
		{"offset jump", leapList(first, transition, 37, 39, expiry), "not one second"},
		{"unordered", leapList(transition, first, 37, 38, expiry), "increasing"},
		{"bad date", leapList(first, transition.Add(24*time.Hour), 37, 38, expiry), "January 1 or July 1"},
		{"duplicate expiry", fmt.Sprintf("#@ %d\n%s", ntpStamp(expiry), leapList(first, transition, 37, 38, expiry)), "duplicate"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.text))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want %q", err, tt.want)
			}
		})
	}
}
