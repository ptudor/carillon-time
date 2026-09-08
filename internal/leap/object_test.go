package leap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func objectForTest(t *testing.T, updated, expiry string, extra string) *Object {
	t.Helper()
	u, err := time.Parse(time.RFC3339, updated)
	if err != nil {
		t.Fatal(err)
	}
	e, err := time.Parse(time.RFC3339, expiry)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte(fmt.Sprintf("#$ %d\n#@ %d\n2272060800 10\n2287785600 11\n%s", dateSeconds(u), dateSeconds(e), extra))
	o, err := NewObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestNISTRenewalAndRollback(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	old := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	renew := objectForTest(t, "2016-07-08T00:00:00Z", "2027-06-28T00:00:00Z", "# renewed\n")
	if err := CheckUpdate(old, renew, now); err != nil {
		t.Fatal(err)
	}
	if old.manifest.Digest == renew.manifest.Digest {
		t.Fatal("renewal must identify new original bytes")
	}
	if err := CheckUpdate(renew, old, now); Reason(err) != "rollback" {
		t.Fatalf("rollback: %v", err)
	}
	conflict, err := NewObject(append(old.Bytes(), []byte("# different bytes\n")...))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckUpdate(old, conflict, now); Reason(err) != "conflict" {
		t.Fatalf("conflict: %v", err)
	}
	if err := CheckUpdate(old, old, old.table.Expiry); Reason(err) != "expired" {
		t.Fatalf("equal digest revived expiry: %v", err)
	}
	if err := CheckUpdate(nil, old, time.Time{}); Reason(err) != "time_unknown" {
		t.Fatalf("bootstrap: %v", err)
	}
	future := objectForTest(t, "2027-01-01T00:00:00Z", "2027-06-28T00:00:00Z", "")
	if err := CheckUpdate(old, future, now); Reason(err) != "date" {
		t.Fatalf("future update: %v", err)
	}
}

func TestHistoryAndExpiredCoverage(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	old := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	july := fmt.Sprintf("%d 12\n", dateSeconds(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)))
	changed := objectForTest(t, "2026-08-01T00:00:00Z", "2027-06-28T00:00:00Z", july)
	if err := CheckUpdate(old, changed, now); Reason(err) != "history" {
		t.Fatalf("covered history changed: %v", err)
	}
	expired := objectForTest(t, "2016-07-08T00:00:00Z", "2026-06-28T00:00:00Z", "")
	if err := CheckUpdate(expired, changed, now); err != nil {
		t.Fatalf("new history after coverage expired: %v", err)
	}
	future := fmt.Sprintf("%d 12\n", dateSeconds(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)))
	renewal := objectForTest(t, "2016-07-08T00:00:00Z", "2027-06-28T00:00:00Z", future)
	if err := CheckUpdate(old, renewal, now); Reason(err) != "history" {
		t.Fatalf("expiry-only revision changed records: %v", err)
	}
}

func TestDateAndObjectBounds(t *testing.T) {
	for _, n := range []uint64{1, 4294967295, 4294967296, 4295053696, 255611289599} {
		d, err := Date(n)
		if err != nil || dateSeconds(d) != n {
			t.Fatalf("%d: %v %v", n, d, err)
		}
	}
	for _, n := range []uint64{0, 255611289600, ^uint64(0)} {
		if _, err := Date(n); err == nil {
			t.Fatalf("date %d accepted", n)
		}
	}
	if _, err := ReadObject(bytes.NewReader(make([]byte, MaxFileSize+1))); err == nil {
		t.Fatal("oversized object accepted")
	}
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	b := o.Bytes()
	b[0] = '!'
	tab := o.Table()
	tab.Transitions[0].At = time.Time{}
	if bytes.Equal(b, o.Bytes()) || o.table.Transitions[0].At.IsZero() {
		t.Fatal("object mutated through accessor")
	}
	if _, err := NewObject([]byte(strings.SplitN(string(o.Bytes()), "\n", 2)[1])); err == nil {
		t.Fatal("undated import accepted")
	}
}

func TestWireGoldenAndBounds(t *testing.T) {
	// Hand-built fixture independent of the encoder, including post-2036
	// integer dates whose high 32 bits must be retained.
	b := make([]byte, FieldSize)
	copy(b, []byte{0xf5, 0x04, 0, 88, 'C', 'L', 'P', 'S', 1, 2, 0, 0})
	b[12] = 7
	binary.BigEndian.PutUint64(b[28:36], 4294967296)
	binary.BigEndian.PutUint64(b[36:44], 4295053696)
	b[44] = 9
	binary.BigEndian.PutUint32(b[76:80], 900)
	m, err := DecodeFields(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Manifest.Updated != 4294967296 || m.Manifest.Expires != 4295053696 {
		t.Fatalf("era lost: %+v", m)
	}
	encoded, err := m.AppendTo(nil)
	if err != nil || !bytes.Equal(b, encoded) {
		t.Fatalf("golden: %x %v", encoded, err)
	}
	for _, offset := range []int{2, 8, 9, 10, 12, 76, 80, 84, 86} {
		bad := bytes.Clone(b)
		bad[offset] = 0xff
		if offset == 12 {
			continue
		} // any nonzero nonce is valid
		if _, err := DecodeFields(bad); err == nil {
			t.Fatalf("bad byte %d accepted", offset)
		}
	}
	if _, err := DecodeFields(append(bytes.Clone(b), b...)); err == nil {
		t.Fatal("duplicate fields accepted")
	}
	if _, err := DecodeFields(b[:87]); err == nil {
		t.Fatal("truncated field accepted")
	}

	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", strings.Repeat("# padding\n", 100))
	var recovered []byte
	for offset := uint32(0); offset < o.manifest.Size; offset += ChunkSize {
		req := Message{Operation: Get, ID: [16]byte{1}, Manifest: o.manifest, Offset: offset, Count: ChunkSize}
		request, err := req.AppendTo(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(request) != 344 {
			t.Fatalf("GET length %d", len(request))
		}
		parsed, err := DecodeFields(request)
		if err != nil {
			t.Fatal(err)
		}
		reply, err := Reply(parsed, o)
		if err != nil {
			t.Fatal(err)
		}
		response, err := reply.AppendTo(nil)
		if err != nil || len(response) > len(request) {
			t.Fatalf("amplification %d %d %v", len(request), len(response), err)
		}
		got, err := DecodeFields(response)
		if err != nil {
			t.Fatal(err)
		}
		recovered = append(recovered, got.Data...)
		request[len(request)-1] = 1
		if _, err := DecodeFields(request); err == nil {
			t.Fatal("nonzero request padding accepted")
		}
	}
	if !bytes.Equal(recovered, o.data) {
		t.Fatal("chunk transfer changed bytes")
	}
}

func TestStoreCrashPointsAndRestart(t *testing.T) {
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	n := objectForTest(t, "2016-07-08T00:00:00Z", "2027-06-28T00:00:00Z", "")
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, stage := range []string{"create", "write", "file_sync", "rename", "directory_sync", "success"} {
		t.Run(stage, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "leap")
			s, err := OpenStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			old := State{Active: &Record{Object: o, Provider: Provider{Kind: "nist", Name: "NIST"}, Accepted: now}, UTCbound: now}
			if err := s.Save(old); err != nil {
				t.Fatal(err)
			}
			if other, err := OpenStore(dir); err == nil {
				other.Close()
				t.Fatal("second writer acquired cache")
			}
			candidate := old
			candidate.Pending = &Record{Object: n, Provider: Provider{Kind: "peer", Name: "home", KeyID: 1}, Accepted: now}
			s.fault = func(at string) error {
				if at == stage {
					return errors.New("injected disk failure")
				}
				return nil
			}
			err = s.Save(candidate)
			if (err == nil) != (stage == "success") {
				t.Fatalf("fault %s: %v", stage, err)
			}
			got, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			if got.Active.Object.manifest != o.manifest || !got.UTCbound.Equal(now) {
				t.Fatal("active generation damaged")
			}
			if (got.Pending != nil) != (stage == "directory_sync" || stage == "success") {
				t.Fatalf("partial generation at %s", stage)
			}
			if got.Pending != nil && got.Anchor().manifest != n.manifest {
				t.Fatal("rollback anchor not coupled to pending data")
			}
		})
	}
	root := t.TempDir()
	if err := os.Symlink(root, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenStore(filepath.Join(root, "link")); err == nil {
		s.Close()
		t.Fatal("symlink cache accepted")
	}
}

func FuzzCLPS(f *testing.F) {
	m := Message{Operation: Probe, ID: [16]byte{1}}
	b, _ := m.AppendTo(nil)
	f.Add(b)
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 1500 {
			return
		}
		m, err := DecodeFields(b)
		if err != nil || m == nil {
			return
		}
		encoded, err := m.AppendTo(nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeFields(encoded); err != nil {
			t.Fatal(err)
		}
		if m.Operation == Probe || m.Operation == Get {
			r, err := Reply(m, nil)
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.AppendTo(nil)
			if err != nil || len(out) > len(b) {
				t.Fatal("reply exceeded request")
			}
		}
	})
}
