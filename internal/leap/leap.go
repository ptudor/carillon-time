// Package leap parses the NIST/IERS leap-seconds.list format and turns its
// transition records into NTP leap indicators for the final UTC day.
package leap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"carillon/internal/ntp"
)

const ntpUnixEpoch = uint64(2_208_988_800)

// Transition is the instant a new TAI-UTC value takes effect and the leap
// warning that applies to the preceding UTC day.
type Transition struct {
	At   time.Time
	Leap ntp.Leap
}

// Table is an immutable leap-seconds.list snapshot.
type Table struct {
	Expiry      time.Time
	Updated     time.Time
	Transitions []Transition
}

// Load opens and parses path.
func Load(path string) (*Table, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("leapfile: open %s: %w", path, err)
	}
	defer f.Close()
	table, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("leapfile %s: %w", path, err)
	}
	return table, nil
}

// Parse reads the standard whitespace-delimited data records plus #@ expiry
// and optional #$ update timestamps. Comments and the optional #h hash are
// accepted; the semantic records themselves are validated strictly.
func Parse(r io.Reader) (*Table, error) {
	table := &Table{}
	type record struct {
		at     time.Time
		offset int
		line   int
	}
	var records []record
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#@") || strings.HasPrefix(line, "#$") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				return nil, fmt.Errorf("line %d: %s timestamp is malformed", lineNo, fields[0])
			}
			stamp, err := parseNTPSeconds(fields[1])
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			if fields[0] == "#@" {
				if !table.Expiry.IsZero() {
					return nil, fmt.Errorf("line %d: duplicate #@ expiry", lineNo)
				}
				table.Expiry = stamp
			} else {
				if !table.Updated.IsZero() {
					return nil, fmt.Errorf("line %d: duplicate #$ update", lineNo)
				}
				table.Updated = stamp
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("line %d: transition record is malformed", lineNo)
		}
		at, err := parseNTPSeconds(fields[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: transition: %w", lineNo, err)
		}
		offset, err := strconv.Atoi(fields[1])
		if err != nil || offset < 1 || offset > 255 {
			return nil, fmt.Errorf("line %d: TAI-UTC offset %q is invalid", lineNo, fields[1])
		}
		if at.Location() != time.UTC || at.Hour() != 0 || at.Minute() != 0 || at.Second() != 0 || at.Day() != 1 || (at.Month() != time.January && at.Month() != time.July) {
			return nil, fmt.Errorf("line %d: transition %s is not midnight on January 1 or July 1 UTC", lineNo, at.Format(time.RFC3339))
		}
		records = append(records, record{at: at, offset: offset, line: lineNo})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if table.Expiry.IsZero() {
		return nil, errors.New("missing #@ expiry timestamp")
	}
	if len(records) < 2 {
		return nil, errors.New("fewer than two transition records")
	}
	if !table.Expiry.After(records[len(records)-1].at) {
		return nil, errors.New("#@ expiry is not after the final transition")
	}
	if !table.Updated.IsZero() && !table.Expiry.After(table.Updated) {
		return nil, errors.New("#@ expiry is not after the #$ update timestamp")
	}
	for i := 1; i < len(records); i++ {
		prev, cur := records[i-1], records[i]
		if !cur.at.After(prev.at) {
			return nil, fmt.Errorf("line %d: transitions are not strictly increasing", cur.line)
		}
		delta := cur.offset - prev.offset
		var indicator ntp.Leap
		switch delta {
		case 1:
			indicator = ntp.LeapInsert
		case -1:
			indicator = ntp.LeapDelete
		default:
			return nil, fmt.Errorf("line %d: TAI-UTC offset changes by %d, not one second", cur.line, delta)
		}
		table.Transitions = append(table.Transitions, Transition{At: cur.at, Leap: indicator})
	}
	return table, nil
}

func parseNTPSeconds(raw string) (time.Time, error) {
	seconds, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || seconds > math.MaxInt64 {
		return time.Time{}, fmt.Errorf("NTP timestamp %q is invalid", raw)
	}
	return time.Unix(int64(seconds)-int64(ntpUnixEpoch), 0).UTC(), nil
}

// Indicator returns the file-authoritative LI value. A warning is active
// only during the 24 hours immediately preceding its transition.
func (t *Table) Indicator(now time.Time) ntp.Leap {
	if t == nil {
		return ntp.LeapNone
	}
	now = now.UTC()
	for _, transition := range t.Transitions {
		start := transition.At.Add(-24 * time.Hour)
		if !now.Before(start) && now.Before(transition.At) {
			return transition.Leap
		}
	}
	return ntp.LeapNone
}

// Crossed reports whether moving forward from before to after passed a leap
// transition. Backward clock changes do not count as a second leap.
func (t *Table) Crossed(before, after time.Time) bool {
	if t == nil || before.IsZero() || !after.After(before) {
		return false
	}
	for _, transition := range t.Transitions {
		if transition.At.After(before) && !transition.At.After(after) {
			return true
		}
	}
	return false
}

// Next returns the first transition strictly after now.
func (t *Table) Next(now time.Time) (Transition, bool) {
	if t != nil {
		for _, transition := range t.Transitions {
			if transition.At.After(now) {
				return transition, true
			}
		}
	}
	return Transition{}, false
}
