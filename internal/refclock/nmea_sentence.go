package refclock

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const maxNMEALine = 1024

type nmeaSentence struct {
	Kind       string
	Timestamp  time.Time
	Valid      bool
	FixQuality int
	Satellites int
	Fractional bool
}

// parseNMEA validates one complete NMEA 0183 sentence and extracts only the
// time/fix fields carillon consumes. The checksum is mandatory.
func parseNMEA(line string) (nmeaSentence, error) {
	line = strings.TrimSpace(line)
	if len(line) == 0 || len(line) > maxNMEALine {
		return nmeaSentence{}, errors.New("NMEA sentence is empty or too long")
	}
	if line[0] != '$' {
		return nmeaSentence{}, errors.New("NMEA sentence does not start with '$'")
	}
	star := strings.LastIndexByte(line, '*')
	if star < 0 || star+3 != len(line) {
		return nmeaSentence{}, errors.New("NMEA checksum is missing or malformed")
	}
	want, err := strconv.ParseUint(line[star+1:], 16, 8)
	if err != nil {
		return nmeaSentence{}, errors.New("NMEA checksum is malformed")
	}
	var got byte
	for i := 1; i < star; i++ {
		got ^= line[i]
	}
	if got != byte(want) {
		return nmeaSentence{}, fmt.Errorf("NMEA checksum mismatch: got %02X want %02X", got, want)
	}
	fields := strings.Split(line[1:star], ",")
	if len(fields) == 0 || len(fields[0]) != 5 {
		return nmeaSentence{}, errors.New("NMEA sentence identifier is malformed")
	}
	talker, kind := fields[0][:2], fields[0][2:]
	switch talker {
	case "GP", "GN", "GL", "GA", "BD":
	default:
		return nmeaSentence{}, fmt.Errorf("unsupported NMEA talker %q", talker)
	}

	switch kind {
	case "RMC":
		if len(fields) < 10 {
			return nmeaSentence{}, errors.New("truncated RMC sentence")
		}
		hour, minute, second, nsec, fractional, err := parseNMEAClock(fields[1])
		if err != nil {
			return nmeaSentence{}, fmt.Errorf("RMC time: %w", err)
		}
		day, month, year, err := parseRMCDate(fields[9])
		if err != nil {
			return nmeaSentence{}, err
		}
		valid := fields[2] == "A"
		if fields[2] != "A" && fields[2] != "V" {
			return nmeaSentence{}, fmt.Errorf("RMC status %q is not A or V", fields[2])
		}
		return nmeaSentence{
			Kind: "RMC", Timestamp: time.Date(year, month, day, hour, minute, second, nsec, time.UTC),
			Valid: valid, Fractional: fractional,
		}, nil

	case "ZDA":
		if len(fields) < 5 {
			return nmeaSentence{}, errors.New("truncated ZDA sentence")
		}
		hour, minute, second, nsec, fractional, err := parseNMEAClock(fields[1])
		if err != nil {
			return nmeaSentence{}, fmt.Errorf("ZDA time: %w", err)
		}
		day, err := parseRange(fields[2], 1, 31, "ZDA day")
		if err != nil {
			return nmeaSentence{}, err
		}
		monthInt, err := parseRange(fields[3], 1, 12, "ZDA month")
		if err != nil {
			return nmeaSentence{}, err
		}
		year, err := parseRange(fields[4], 2000, 9999, "ZDA year")
		if err != nil {
			return nmeaSentence{}, err
		}
		stamp := time.Date(year, time.Month(monthInt), day, hour, minute, second, nsec, time.UTC)
		if stamp.Day() != day || stamp.Month() != time.Month(monthInt) || stamp.Year() != year {
			return nmeaSentence{}, errors.New("ZDA date is not a calendar date")
		}
		return nmeaSentence{Kind: "ZDA", Timestamp: stamp, Valid: true, Fractional: fractional}, nil

	case "GGA":
		if len(fields) < 8 {
			return nmeaSentence{}, errors.New("truncated GGA sentence")
		}
		quality, err := parseRange(fields[6], 0, 8, "GGA fix quality")
		if err != nil {
			return nmeaSentence{}, err
		}
		satellites, err := parseRange(fields[7], 0, 99, "GGA satellites")
		if err != nil {
			return nmeaSentence{}, err
		}
		return nmeaSentence{Kind: "GGA", Valid: quality > 0, FixQuality: quality, Satellites: satellites}, nil

	default:
		return nmeaSentence{}, fmt.Errorf("unsupported NMEA sentence %q", kind)
	}
}

func parseNMEAClock(raw string) (hour, minute, second, nsec int, fractional bool, err error) {
	whole, frac, _ := strings.Cut(raw, ".")
	if len(whole) != 6 {
		return 0, 0, 0, 0, false, errors.New("must be hhmmss with an optional fraction")
	}
	hour, err = parseDigits(whole[0:2])
	if err != nil || hour > 23 {
		return 0, 0, 0, 0, false, errors.New("hour is invalid")
	}
	minute, err = parseDigits(whole[2:4])
	if err != nil || minute > 59 {
		return 0, 0, 0, 0, false, errors.New("minute is invalid")
	}
	second, err = parseDigits(whole[4:6])
	if err != nil || second > 60 {
		return 0, 0, 0, 0, false, errors.New("second is invalid")
	}
	if second == 60 {
		// UTC second 60 is a leap second, and time.Time cannot represent it
		// distinctly: time.Date normalises 23:59:60 into the following
		// midnight. Accepting it would silently produce an ordinary sample
		// dated one second late, colliding with the real next second in the
		// duplicate watermark. RMC validated its calendar date before the
		// normalisation and so let it through, while ZDA validated the
		// normalised date and rejected it — the same instant treated two
		// different ways (RA6X-042).
		//
		// Both are now refused. Losing one sentence per leap second costs
		// nothing: the offset window is 16 samples deep, the reach register
		// tolerates a gap, and the kernel applies the leap itself while the
		// engine handles the boundary (RA6X-007).
		if hour == 23 && minute == 59 {
			return 0, 0, 0, 0, false, errors.New("leap second 23:59:60 has no representable instant")
		}
		return 0, 0, 0, 0, false, errors.New("second 60 outside a leap-second boundary")
	}
	if frac == "" {
		return hour, minute, second, 0, false, nil
	}
	// Validate the whole fractional field before truncating to the supported
	// precision: cutting it first let ".123456789abc" through as 123456789
	// nanoseconds.
	if _, err := parseDigits(frac); err != nil {
		return 0, 0, 0, 0, false, errors.New("fraction is invalid")
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	for len(frac) < 9 {
		frac += "0"
	}
	nsec, err = parseDigits(frac)
	if err != nil {
		return 0, 0, 0, 0, false, errors.New("fraction is invalid")
	}
	return hour, minute, second, nsec, nsec != 0, nil
}

func parseRMCDate(raw string) (day int, month time.Month, year int, err error) {
	if len(raw) != 6 {
		return 0, 0, 0, errors.New("RMC date must be ddmmyy")
	}
	day, err = parseRange(raw[:2], 1, 31, "RMC day")
	if err != nil {
		return 0, 0, 0, err
	}
	monthInt, err := parseRange(raw[2:4], 1, 12, "RMC month")
	if err != nil {
		return 0, 0, 0, err
	}
	yy, err := parseRange(raw[4:], 0, 99, "RMC year")
	if err != nil {
		return 0, 0, 0, err
	}
	year, month = 2000+yy, time.Month(monthInt)
	stamp := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if stamp.Day() != day || stamp.Month() != month || stamp.Year() != year {
		return 0, 0, 0, errors.New("RMC date is not a calendar date")
	}
	return day, month, year, nil
}

func parseRange(raw string, low, high int, field string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < low || n > high {
		return 0, fmt.Errorf("%s %q is invalid", field, raw)
	}
	return n, nil
}

func parseDigits(raw string) (int, error) {
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0, errors.New("not decimal digits")
		}
	}
	return strconv.Atoi(raw)
}
