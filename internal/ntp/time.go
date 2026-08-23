// Package ntp implements the NTPv4 on-wire format of RFC 5905: 64-bit and
// 32-bit timestamps, the 48-byte packet header, kiss-o'-death codes, and the
// extension-field / MAC framing rules of RFC 7822. It is pure and does no I/O.
package ntp

import (
	"crypto/md5"
	"math"
	"net/netip"
	"time"
)

const (
	// Port is the well-known NTP UDP port.
	Port = 123

	// unixEpochOffset is the number of seconds between the NTP prime epoch
	// (1900-01-01T00:00:00Z) and the Unix epoch (1970-01-01T00:00:00Z).
	unixEpochOffset = 2208988800

	// eraLength is the length of one NTP era in seconds (2^32).
	eraLength = int64(1) << 32
)

// Time is a 64-bit NTP timestamp: the high 32 bits are seconds since the
// prime epoch modulo 2^32, the low 32 bits are the binary fraction of a
// second. The zero value is the protocol's "unknown/unavailable" timestamp.
type Time uint64

// FromUnix converts Unix seconds and nanoseconds to an NTP timestamp. The era
// is discarded (modulo 2^32), as the wire format requires; Unix(near) or
// Sub recover it. nsec must be in [0, 1e9).
func FromUnix(sec, nsec int64) Time {
	s := uint32(uint64(sec + unixEpochOffset)) // deliberate wrap into the era
	f := uint32((nsec << 32) / 1e9)            // nsec < 1e9, so nsec<<32 fits in int64
	return Time(uint64(s)<<32 | uint64(f))
}

// FromTime converts a time.Time to an NTP timestamp.
func FromTime(t time.Time) Time {
	return FromUnix(t.Unix(), int64(t.Nanosecond()))
}

// Seconds returns the integer-seconds field.
func (t Time) Seconds() uint32 { return uint32(t >> 32) }

// Fraction returns the 32-bit fraction-of-a-second field.
func (t Time) Fraction() uint32 { return uint32(t) }

// IsZero reports whether t is the protocol's null timestamp.
func (t Time) IsZero() bool { return t == 0 }

// Unix returns t as Unix seconds and nanoseconds. NTP timestamps carry no era,
// so the era is chosen to place the result within ±68 years of nearSec, which
// callers pass as the current local time (RFC 5905 §6). The fraction is
// truncated, not rounded, so nsec is always in [0, 1e9).
func (t Time) Unix(nearSec int64) (sec, nsec int64) {
	sec = int64(t.Seconds()) - unixEpochOffset // era 0
	diff := nearSec - sec
	eras := floorDiv(diff+eraLength/2, eraLength)
	sec += eras * eraLength
	nsec = (int64(t.Fraction()) * 1e9) >> 32
	return sec, nsec
}

// Time returns t as a time.Time in the era closest to near.
func (t Time) Time(near time.Time) time.Time {
	sec, nsec := t.Unix(near.Unix())
	return time.Unix(sec, nsec)
}

// Sub returns t − u in seconds. The subtraction is done modulo 2^64 and
// interpreted as signed, so it is correct across an era boundary as long as
// the true difference is within ±68 years.
func (t Time) Sub(u Time) float64 {
	d := int64(t - u) // wraps by design
	return float64(d) / float64(eraLength)
}

// Add returns t advanced by d seconds (d may be negative).
func (t Time) Add(d float64) Time {
	return Time(uint64(int64(t) + int64(math.Round(d*float64(eraLength)))))
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// Short is the 32-bit NTP short format used for root delay and root
// dispersion: 16 bits of seconds, 16 bits of fraction, unsigned.
type Short uint32

// ShortFromSeconds converts seconds to the short format, saturating at both
// ends: negative values become 0 and values ≥ 65536 become the maximum.
func ShortFromSeconds(s float64) Short {
	if s <= 0 || math.IsNaN(s) {
		return 0
	}
	if s >= 65536 {
		return math.MaxUint32
	}
	return Short(s * 65536)
}

// Seconds converts the short format to seconds.
func (s Short) Seconds() float64 { return float64(s) / 65536 }

// Log2Seconds converts a log2 exponent (the packet precision/poll fields) to
// seconds.
func Log2Seconds(exp int8) float64 { return math.Ldexp(1, int(exp)) }

// PrecisionFromSeconds returns the log2 precision field for a clock whose
// resolution is s seconds, rounded down, clamped to the range NTP daemons
// conventionally report.
func PrecisionFromSeconds(s float64) int8 {
	if s <= 0 || math.IsNaN(s) {
		return -30
	}
	p := math.Floor(math.Log2(s))
	switch {
	case p < -30:
		return -30
	case p > -6:
		return -6
	}
	return int8(p)
}

// Leap is the two-bit leap indicator.
type Leap uint8

const (
	LeapNone   Leap = 0 // no warning
	LeapInsert Leap = 1 // last minute of the day has 61 seconds
	LeapDelete Leap = 2 // last minute of the day has 59 seconds
	LeapUnsync Leap = 3 // clock unsynchronized
)

func (l Leap) String() string {
	switch l {
	case LeapNone:
		return "none"
	case LeapInsert:
		return "insert"
	case LeapDelete:
		return "delete"
	default:
		return "unsynchronized"
	}
}

// Mode is the three-bit association mode.
type Mode uint8

const (
	ModeReserved         Mode = 0
	ModeSymmetricActive  Mode = 1
	ModeSymmetricPassive Mode = 2
	ModeClient           Mode = 3
	ModeServer           Mode = 4
	ModeBroadcast        Mode = 5
	ModeControl          Mode = 6
	ModePrivate          Mode = 7
)

func (m Mode) String() string {
	switch m {
	case ModeSymmetricActive:
		return "symmetric-active"
	case ModeSymmetricPassive:
		return "symmetric-passive"
	case ModeClient:
		return "client"
	case ModeServer:
		return "server"
	case ModeBroadcast:
		return "broadcast"
	case ModeControl:
		return "control"
	case ModePrivate:
		return "private"
	default:
		return "reserved"
	}
}

// RefID is the 32-bit reference identifier. At stratum 0 it is a kiss code,
// at stratum 1 a four-character ASCII clock identifier, and above that the
// IPv4 address or the first four bytes of the MD5 of the IPv6 address of the
// system source.
type RefID [4]byte

// RefIDFromString builds a text reference id ("GPS", "PPS", "RATE"),
// space-padded on the right.
func RefIDFromString(s string) RefID {
	var r RefID
	for i := range r {
		if i < len(s) {
			r[i] = s[i]
		} else {
			r[i] = ' '
		}
	}
	return r
}

// RefIDFromAddr builds the reference id for a source address per RFC 5905
// §7.3: the IPv4 address itself, or MD5(IPv6 address)[0:4].
func RefIDFromAddr(a netip.Addr) RefID {
	a = a.Unmap()
	if a.Is4() {
		return RefID(a.As4())
	}
	sum := md5.Sum(a.AsSlice())
	var r RefID
	copy(r[:], sum[:4])
	return r
}

// IsText reports whether all four bytes are printable ASCII, which is how a
// kiss code or stratum-1 identifier is distinguished from an address.
func (r RefID) IsText() bool {
	for _, b := range r {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

// Text returns the identifier as a string with trailing spaces removed. It
// is only meaningful when IsText is true.
func (r RefID) Text() string {
	n := 4
	for n > 0 && r[n-1] == ' ' {
		n--
	}
	return string(r[:n])
}

// String renders text identifiers as text and everything else in dotted
// form, which is how ntpd/chrony display an IPv4 or hashed-IPv6 refid.
func (r RefID) String() string {
	if r.IsText() {
		return r.Text()
	}
	return netip.AddrFrom4(r).String()
}

// Kiss-o'-death codes (RFC 5905 §7.4) and the unsynchronized-server
// identifiers this daemon sends at stratum 16.
var (
	KissRATE = RefIDFromString("RATE") // rate exceeded; slow down
	KissDENY = RefIDFromString("DENY") // access denied; stop sending
	KissRSTR = RefIDFromString("RSTR") // access restricted; stop sending
	KissINIT = RefIDFromString("INIT") // not yet synchronized
	KissSTEP = RefIDFromString("STEP") // just stepped; resynchronizing
	KissHOLD = RefIDFromString("HOLD") // holdover expired (carillon-specific)
)
