package leap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
	"golang.org/x/sys/unix"
)

const MaxFileSize = 65536

// Manifest identifies original file bytes. Dates are full seconds since 1900,
// not the NTP header's 32.32 timestamps. No era inference is performed.
type Manifest struct {
	Updated uint64   `json:"data_updated"`
	Expires uint64   `json:"expires"`
	Digest  [32]byte `json:"sha256"`
	Size    uint32   `json:"size"`
}

func (m Manifest) Hash() string { return hex.EncodeToString(m.Digest[:]) }

func Date(seconds uint64) (time.Time, error) {
	// RFC3339 and JSON use four-digit years. Check before uint64 -> int64.
	const maxDate = uint64(255611289599) // 9999-12-31T23:59:59Z, NTP epoch
	if seconds == 0 || seconds > maxDate {
		return time.Time{}, fmt.Errorf("leap: date %d is outside 1900..9999", seconds)
	}
	return time.Unix(int64(seconds)-int64(ntpUnixEpoch), 0).UTC(), nil
}

func dateSeconds(t time.Time) uint64 { return uint64(t.Unix() + int64(ntpUnixEpoch)) }

func (m Manifest) Validate() error {
	u, err := Date(m.Updated)
	if err != nil {
		return err
	}
	e, err := Date(m.Expires)
	if err != nil {
		return err
	}
	if !e.After(u) || m.Size == 0 || m.Size > MaxFileSize || m.Digest == ([32]byte{}) {
		return errors.New("leap: invalid manifest")
	}
	return nil
}

// Object is immutable after construction. Bytes and Table return copies so
// an asynchronous receiver cannot modify an object already being served.
type Object struct {
	manifest Manifest
	table    Table
	data     []byte
}

func NewObject(data []byte) (*Object, error) {
	if len(data) == 0 || len(data) > MaxFileSize {
		return nil, Reject("size", "file exceeds the 64 KiB bound or is empty")
	}
	t, err := Parse(bytes.NewReader(data))
	if err != nil {
		return nil, Reject("parse", err.Error())
	}
	if t.Updated.IsZero() {
		return nil, Reject("metadata", "missing #$ data-update date")
	}
	o := &Object{data: bytes.Clone(data), table: *t}
	o.manifest = Manifest{Updated: dateSeconds(t.Updated), Expires: dateSeconds(t.Expiry), Digest: sha256.Sum256(data), Size: uint32(len(data))}
	return o, nil
}

func ReadObject(r io.Reader) (*Object, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("leap: read object: %w", err)
	}
	return NewObject(b)
}

func LoadObject(path string) (*Object, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("leap: open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("leap: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("leap: %s is not a regular file", path)
	}
	o, err := ReadObject(f)
	if err != nil {
		return nil, fmt.Errorf("leap: %s: %w", path, err)
	}
	return o, nil
}

func (o *Object) Manifest() Manifest               { return o.manifest }
func (o *Object) Expiry() time.Time                { return o.table.Expiry }
func (o *Object) Updated() time.Time               { return o.table.Updated }
func (o *Object) Indicator(now time.Time) ntp.Leap { return o.table.Indicator(now) }
func (o *Object) Bytes() []byte                    { return bytes.Clone(o.data) }
func (o *Object) Table() *Table {
	t := o.table
	t.Transitions = slices.Clone(t.Transitions)
	return &t
}

// Rejection has a bounded reason for logs, status and metric labels.
type Rejection struct{ Reason, Detail string }

func (e *Rejection) Error() string       { return "leap: " + e.Reason + ": " + e.Detail }
func Reject(reason, detail string) error { return &Rejection{reason, detail} }
func Reason(err error) string {
	var r *Rejection
	if errors.As(err, &r) {
		return r.Reason
	}
	return "io"
}

// CheckUpdate validates time coverage and the revision against a durable
// anchor. Returning nil for an equal digest does not extend its expiry.
func CheckUpdate(old, next *Object, now time.Time) error {
	if next == nil {
		return Reject("metadata", "missing object")
	}
	if now.IsZero() {
		return Reject("time_unknown", "UTC has not been established")
	}
	n := &next.table
	if !now.Before(n.Expiry) {
		return Reject("expired", "table no longer covers UTC")
	}
	if now.Before(n.Baseline) || n.Updated.After(now.Add(5*time.Minute)) || n.Expiry.After(now.Add(400*24*time.Hour)) {
		return Reject("date", "table dates are outside the accepted UTC bounds")
	}
	if old == nil {
		return nil
	}
	o := &old.table
	if next.manifest == old.manifest {
		return nil
	}
	if n.Updated.Before(o.Updated) || n.Expiry.Before(o.Expiry) {
		return Reject("rollback", "data-update or expiry date decreased")
	}
	if n.Updated.Equal(o.Updated) && n.Expiry.Equal(o.Expiry) {
		return Reject("conflict", "different bytes with identical dates")
	}
	if !n.Baseline.Equal(o.Baseline) || n.Offset != o.Offset {
		return Reject("history", "TAI-UTC baseline changed")
	}
	if n.Updated.Equal(o.Updated) && !slices.Equal(n.Transitions, o.Transitions) {
		return Reject("history", "expiry-only renewal changed leap records")
	}
	// Preserve the interval the old table covered. New history after an old
	// table expired can be learned, but never executed retroactively.
	cutoff := now
	if !cutoff.Before(o.Expiry) {
		cutoff = o.Expiry.Add(-time.Nanosecond)
	}
	past := func(t *Table) []Transition {
		i := 0
		for i < len(t.Transitions) && !t.Transitions[i].At.After(cutoff) {
			i++
		}
		return t.Transitions[:i]
	}
	if !slices.Equal(past(o), past(n)) {
		return Reject("history", "effective leap history changed")
	}
	return nil
}
