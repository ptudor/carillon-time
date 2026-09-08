package server

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/leap"
	"github.com/ptudor/carillon-time/internal/ntp"
	"github.com/ptudor/carillon-time/internal/ntp/auth"
)

func serverLeapObject(t *testing.T) *leap.Object {
	t.Helper()
	b := []byte("#$ 3676924800\n#@ 4007404800\n2272060800 10\n2287785600 11\n")
	b = append(b, bytes.Repeat([]byte("# original comment\n"), 40)...)
	o, err := leap.NewObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func leapRequest(t *testing.T, m leap.Message, key *auth.Key) []byte {
	t.Helper()
	b, err := m.AppendTo(request(4))
	if err != nil {
		t.Fatal(err)
	}
	if key != nil {
		b = key.Append(b)
	}
	return b
}

func TestLeapExportAuthorizationAndAmplification(t *testing.T) {
	o := serverLeapObject(t)
	for _, kind := range []string{"authorized", "unsigned", "time_key_only", "wrong_key", "bad_mac", "denied_acl", "malformed", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			other := auth.Key{ID: 2, Secret: []byte("fedcba9876543210")}
			d := leap.NewDistributor([]uint32{1}, func() *leap.Object { return o }, nil)
			h, _ := newTestHandler(t, func(c *Config) {
				c.Leap = d
				c.Keys[2] = other
				if kind == "denied_acl" {
					c.Deny = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
				}
			})
			m := leap.Message{Operation: leap.Probe, ID: [16]byte{1}}
			key := &testKey
			if kind == "unsigned" {
				key = nil
			}
			if kind == "time_key_only" {
				key = &other
			}
			if kind == "wrong_key" {
				k := other
				k.ID = 3
				key = &k
			}
			b := leapRequest(t, m, key)
			if kind == "bad_mac" {
				b[len(b)-1] ^= 1
			}
			if kind == "malformed" {
				b[48+86] = 1
				b = testKey.Append(b[:len(b)-20])
			}
			if kind == "legacy" {
				b = testKey.Append(request(3))
			}
			out := h.Handle(b, from("192.0.2.9"), testWall, time.Now())
			if len(out) > len(b) {
				t.Fatalf("amplifier: %d -> %d", len(b), len(out))
			}
			if kind == "denied_acl" || kind == "malformed" {
				if out != nil {
					t.Fatal("bad request received a reply")
				}
				return
			}
			_, mac, offset, err := ntp.Decode(out)
			if err != nil {
				t.Fatal(err)
			}
			reply, err := leap.DecodeFields(out[48:offset])
			if err != nil {
				t.Fatal(err)
			}
			if kind != "authorized" {
				if reply != nil {
					t.Fatal("unauthorized export")
				}
				return
			}
			if !testKey.Verify(out[:offset], mac) || reply == nil || reply.Manifest != o.Manifest() {
				t.Fatal("manifest missing or unauthenticated")
			}
			for offset := uint32(0); offset < o.Manifest().Size; offset += leap.ChunkSize {
				m = leap.Message{Operation: leap.Get, ID: [16]byte{2}, Manifest: o.Manifest(), Offset: offset, Count: leap.ChunkSize}
				b = leapRequest(t, m, &testKey)
				out = h.Handle(b, from("192.0.2.9"), testWall, time.Now().Add(time.Duration(offset/leap.ChunkSize+1)*4*time.Second))
				if len(out) > len(b) {
					t.Fatal("chunk response amplified")
				}
				_, mac, end, err := ntp.Decode(out)
				if err != nil {
					t.Fatal(err)
				}
				if !testKey.Verify(out[:end], mac) {
					t.Fatal("chunk MAC missing")
				}
				got, err := leap.DecodeFields(out[48:end])
				if err != nil {
					t.Fatal(err)
				}
				if got == nil || !bytes.Equal(got.Data, o.Bytes()[offset:offset+uint32(got.Count)]) {
					t.Fatal("chunk bytes changed")
				}
			}
		})
	}
}

func TestLeapBudgetsSharedAcrossListenersAndSeparateFromTime(t *testing.T) {
	o := serverLeapObject(t)
	counts := new(leap.Counters)
	d := leap.NewDistributor([]uint32{1}, func() *leap.Object { return o }, counts)
	h1, _ := newTestHandler(t, func(c *Config) { c.Leap = d })
	h2, _ := newTestHandler(t, func(c *Config) { c.Leap = d })
	mono := time.Now()
	b := leapRequest(t, leap.Message{Operation: leap.Probe, ID: [16]byte{1}}, &testKey)
	if h1.Handle(b, from("192.0.2.1"), testWall, mono) == nil || h2.Handle(b, from("192.0.2.2"), testWall, mono) == nil {
		t.Fatal("initial shared burst not available")
	}
	if h1.Handle(b, from("192.0.2.3"), testWall, mono) != nil {
		t.Fatal("changing address bypassed per-key budget")
	}
	if h2.Handle(testKey.Append(request(4)), from("192.0.2.3"), testWall, mono) == nil {
		t.Fatal("leap budget exhausted ordinary time service")
	}
	if counts.RateLimited.Load() != 1 {
		t.Fatal("leap budget refusal not counted")
	}
	if h1.Handle(b, from("192.0.2.1"), testWall, mono.Add(4*time.Second)) == nil {
		t.Fatal("budget failed to refill")
	}
}

func TestExpiredDistributorAndObjectReplacement(t *testing.T) {
	o := serverLeapObject(t)
	current := o
	d := leap.NewDistributor([]uint32{1}, func() *leap.Object { return current }, nil)
	wall := o.Expiry()
	h, _ := newTestHandler(t, func(c *Config) { c.Leap = d; c.Now = func() time.Time { return wall } })
	req := leap.Message{Operation: leap.Probe, ID: [16]byte{1}}
	for _, op := range []uint8{leap.Probe, leap.Get} {
		req.Operation = op
		if op == leap.Get {
			req.Manifest = o.Manifest()
			req.Count = leap.ChunkSize
		}
		b := leapRequest(t, req, &testKey)
		out := h.Handle(b, from("192.0.2.1"), wall, time.Now())
		_, _, off, err := ntp.Decode(out)
		if err != nil {
			t.Fatal(err)
		}
		m, err := leap.DecodeFields(out[48:off])
		if err != nil {
			t.Fatal(err)
		}
		if op == leap.Probe && m.Result != leap.NoTable {
			t.Fatal("expired manifest advertised")
		}
		if op == leap.Get && m.Result != leap.NotFound {
			t.Fatal("expired bytes served")
		}
	}
}
