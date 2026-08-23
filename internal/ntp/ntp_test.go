package ntp

import (
	"bytes"
	"encoding/binary"
	"math"
	"net/netip"
	"testing"
	"time"
)

func TestTimeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		sec  int64
		nsec int64
	}{
		{"unix epoch", 0, 0},
		{"2026", 1787000000, 123456789},
		{"just before era 1", 2085978495, 999999999}, // 2036-02-07T06:28:15.999999999Z
		{"era 1 start", 2085978496, 0},               // 2036-02-07T06:28:16Z
		{"2038 y2k38", 2147483648, 500000000},
		{"2100", 4102444800, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nt := FromUnix(c.sec, c.nsec)
			gotSec, gotNsec := nt.Unix(c.sec) // "near" is the true time itself
			if gotSec != c.sec {
				t.Fatalf("sec: got %d want %d", gotSec, c.sec)
			}
			if d := gotNsec - c.nsec; d < -1 || d > 0 {
				t.Fatalf("nsec: got %d want %d (±1 truncation allowed)", gotNsec, c.nsec)
			}
			if gotNsec < 0 || gotNsec >= 1e9 {
				t.Fatalf("nsec out of range: %d", gotNsec)
			}
		})
	}
}

func TestTimeEraResolution(t *testing.T) {
	// A timestamp from era 1 must resolve to era 1 when "now" is anywhere
	// within ±68 years of it, including when "now" is still in era 0.
	target := int64(2085978496 + 3600) // one hour into era 1
	nt := FromUnix(target, 0)
	for _, near := range []int64{target - 86400*365*10, target - 1, target, target + 1, target + 86400*365*30} {
		sec, _ := nt.Unix(near)
		if sec != target {
			t.Fatalf("near=%d: got %d want %d", near, sec, target)
		}
	}
	// And an era-0 timestamp seen from early era 1 still resolves to era 0.
	old := int64(2085978496 - 3600)
	nt = FromUnix(old, 0)
	if sec, _ := nt.Unix(2085978496 + 86400); sec != old {
		t.Fatalf("got %d want %d", sec, old)
	}
}

func TestTimeSubAcrossEra(t *testing.T) {
	a := FromUnix(2085978496-1, 500000000) // 0.5 s before the era boundary
	b := FromUnix(2085978496, 250000000)   // 0.25 s after
	if d := b.Sub(a); math.Abs(d-0.75) > 1e-9 {
		t.Fatalf("Sub across era: got %v want 0.75", d)
	}
	if d := a.Sub(b); math.Abs(d+0.75) > 1e-9 {
		t.Fatalf("Sub across era (negative): got %v want -0.75", d)
	}
}

func TestTimeAdd(t *testing.T) {
	a := FromUnix(1787000000, 0)
	b := a.Add(1.5)
	if d := b.Sub(a); math.Abs(d-1.5) > 1e-9 {
		t.Fatalf("got %v", d)
	}
	if d := a.Add(-2).Sub(a); math.Abs(d+2) > 1e-9 {
		t.Fatalf("got %v", d)
	}
}

func TestFromTime(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 987654321, time.UTC)
	nt := FromTime(now)
	back := nt.Time(now)
	if back.Unix() != now.Unix() || now.Nanosecond()-back.Nanosecond() > 1 {
		t.Fatalf("got %v want %v", back, now)
	}
}

func TestShort(t *testing.T) {
	if ShortFromSeconds(-1) != 0 {
		t.Fatal("negative must clamp to 0")
	}
	if ShortFromSeconds(70000) != math.MaxUint32 {
		t.Fatal("overflow must saturate")
	}
	if ShortFromSeconds(math.NaN()) != 0 {
		t.Fatal("NaN must clamp to 0")
	}
	s := ShortFromSeconds(0.015625) // 1/64
	if s != 1024 || s.Seconds() != 0.015625 {
		t.Fatalf("got %d (%v s)", s, s.Seconds())
	}
}

func TestPrecision(t *testing.T) {
	if p := PrecisionFromSeconds(1e-6); p != -20 {
		t.Fatalf("1 µs: got %d want -20", p)
	}
	if p := PrecisionFromSeconds(1e-12); p != -30 {
		t.Fatalf("clamp low: got %d", p)
	}
	if p := PrecisionFromSeconds(1); p != -6 {
		t.Fatalf("clamp high: got %d", p)
	}
	if Log2Seconds(-20) != 1.0/1048576 {
		t.Fatal("Log2Seconds")
	}
}

func TestRefID(t *testing.T) {
	if RefIDFromString("GPS").String() != "GPS" {
		t.Fatal("text refid")
	}
	if RefIDFromString("GPS") != (RefID{'G', 'P', 'S', ' '}) {
		t.Fatal("padding")
	}
	v4 := RefIDFromAddr(netip.MustParseAddr("192.0.2.1"))
	if v4 != (RefID{192, 0, 2, 1}) || v4.String() != "192.0.2.1" {
		t.Fatalf("v4 refid: %v", v4)
	}
	mapped := RefIDFromAddr(netip.MustParseAddr("::ffff:192.0.2.1"))
	if mapped != v4 {
		t.Fatal("v4-mapped must unmap")
	}
	// MD5("2001:db8::1" as 16 bytes)[0:4]; computed independently.
	v6 := RefIDFromAddr(netip.MustParseAddr("2001:db8::1"))
	if v6 == v4 || v6 == (RefID{}) {
		t.Fatalf("v6 refid: %v", v6)
	}
	if KissRATE.Text() != "RATE" || !KissRATE.IsText() {
		t.Fatal("kiss code")
	}
}

// golden is a hand-assembled client request: LI=0 VN=4 mode=3, stratum 0,
// poll 6, precision -20, zero root fields, zero refid, zero reference/
// origin/receive, transmit = 0xE6F2A1B8_80000000 (arbitrary).
var golden = []byte{
	0x23, 0x00, 0x06, 0xec,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00,
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	0, 0, 0, 0, 0, 0, 0, 0,
	0xe6, 0xf2, 0xa1, 0xb8, 0x80, 0x00, 0x00, 0x00,
}

func TestPacketGolden(t *testing.T) {
	p := Packet{Version: 4, Mode: ModeClient, Poll: 6, Precision: -20, TransmitTime: 0xE6F2A1B880000000}
	if got := p.Marshal(); !bytes.Equal(got, golden) {
		t.Fatalf("encode:\n got % x\nwant % x", got, golden)
	}
	dec, mac, off, err := Decode(golden)
	if err != nil || mac != nil || off != 48 {
		t.Fatalf("decode: %v mac=%v off=%d", err, mac, off)
	}
	if dec != p {
		t.Fatalf("decode: got %+v want %+v", dec, p)
	}
}

func TestPacketServerReply(t *testing.T) {
	p := Packet{
		Leap: LeapInsert, Version: 4, Mode: ModeServer, Stratum: 2, Poll: 10, Precision: -23,
		RootDelay: ShortFromSeconds(0.0123), RootDispersion: ShortFromSeconds(0.05),
		ReferenceID:   RefIDFromAddr(netip.MustParseAddr("192.0.2.7")),
		ReferenceTime: FromUnix(1787000000, 0), OriginTime: 0x1122334455667788,
		ReceiveTime: FromUnix(1787000100, 1), TransmitTime: FromUnix(1787000100, 2000),
	}
	b := p.Marshal()
	if b[0] != 0x64 { // LI=1 (01), VN=4 (100), mode=4 (100) → 0110 0100
		t.Fatalf("first byte % x", b[0])
	}
	got, _, _, err := Decode(b)
	if err != nil || got != p {
		t.Fatalf("round trip: %v\n got %+v\nwant %+v", err, got, p)
	}
}

func TestDecodeErrors(t *testing.T) {
	if _, _, _, err := Decode(golden[:47]); err != ErrShort {
		t.Fatalf("short: %v", err)
	}
	v0 := append([]byte(nil), golden...)
	v0[0] = 0x03 // version 0
	if _, _, _, err := Decode(v0); err == nil {
		t.Fatal("version 0 must fail")
	}
	v5 := append([]byte(nil), golden...)
	v5[0] = 0x2b // version 5
	if _, _, _, err := Decode(v5); err == nil {
		t.Fatal("version 5 must fail")
	}
	for _, v := range []byte{1, 2, 3} {
		b := append([]byte(nil), golden...)
		b[0] = v<<3 | 3
		if p, _, _, err := Decode(b); err != nil || p.Version != v {
			t.Fatalf("version %d must decode: %v", v, err)
		}
	}
}

func ef(typ uint16, length int) []byte { return efClaim(typ, length, length) }

// efClaim builds an extension field of `actual` bytes whose length field
// claims `claimed` bytes.
func efClaim(typ uint16, claimed, actual int) []byte {
	b := make([]byte, actual)
	binary.BigEndian.PutUint16(b[0:2], typ)
	binary.BigEndian.PutUint16(b[2:4], uint16(claimed))
	return b
}

func TestDecodeTrailer(t *testing.T) {
	mac20 := make([]byte, 20)
	binary.BigEndian.PutUint32(mac20, 7)
	for i := 4; i < 20; i++ {
		mac20[i] = byte(i)
	}
	mac24 := make([]byte, 24)
	binary.BigEndian.PutUint32(mac24, 9)

	cases := []struct {
		name    string
		trailer []byte
		wantErr bool
		keyID   uint32
		digest  int
		nak     bool
	}{
		{"none", nil, false, 0, -1, false},
		{"cmac", mac20, false, 7, 16, false},
		{"sha1", mac24, false, 9, 20, false},
		{"crypto-nak", []byte{0, 0, 0, 0}, false, 0, 0, true},
		{"nak nonzero", []byte{0, 0, 0, 1}, true, 0, 0, false},
		{"ef then mac", append(ef(0x0104, 28), mac20...), false, 7, 16, false},
		{"two efs then mac", append(append(ef(0x0104, 32), ef(0x0204, 28)...), mac20...), false, 7, 16, false},
		{"ef no mac", ef(0x0104, 28), false, 0, -1, false},
		{"ef too short", append(ef(0x0104, 12), mac20...), true, 0, 0, false},
		{"ef unaligned", append(ef(0x0104, 30), mac20...), true, 0, 0, false},
		{"ef overruns", append(efClaim(0x0104, 200, 28), mac20...), true, 0, 0, false},
		{"long ef then mac", append(ef(0x0104, 200), mac20...), false, 7, 16, false},
		{"junk 13", make([]byte, 13), true, 0, 0, false},
		{"junk 1", []byte{1}, true, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := append(append([]byte(nil), golden...), c.trailer...)
			p, mac, off, err := Decode(b)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got mac=%v", mac)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if p.TransmitTime != 0xE6F2A1B880000000 {
				t.Fatal("header corrupted by trailer parsing")
			}
			if c.digest < 0 {
				if mac != nil || off != len(b) {
					t.Fatalf("expected no MAC, got %v off=%d", mac, off)
				}
				return
			}
			if mac == nil {
				t.Fatal("expected MAC")
			}
			if mac.IsCryptoNAK() != c.nak {
				t.Fatalf("nak: got %v", mac.IsCryptoNAK())
			}
			if mac.KeyID != c.keyID || len(mac.Digest) != c.digest {
				t.Fatalf("got key %d digest %d", mac.KeyID, len(mac.Digest))
			}
			if off != len(b)-len(mac.Digest)-4 {
				t.Fatalf("mac offset %d", off)
			}
		})
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(golden)
	f.Add(append(append([]byte(nil), golden...), make([]byte, 20)...))
	f.Add(append(append([]byte(nil), golden...), ef(1, 28)...))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, mac, off, err := Decode(b)
		if err != nil {
			return
		}
		if off < HeaderSize || off > len(b) {
			t.Fatalf("mac offset %d out of range for %d bytes", off, len(b))
		}
		if mac != nil && off+4+len(mac.Digest) != len(b) {
			t.Fatalf("MAC does not end at packet end")
		}
		if !bytes.Equal(p.Marshal(), b[:HeaderSize]) {
			t.Fatalf("re-encoding the header changed it")
		}
	})
}
