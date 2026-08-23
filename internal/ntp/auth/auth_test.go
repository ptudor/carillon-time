package auth

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carillon/internal/ntp"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatalf("bad hex in test: %v", err)
	}
	return b
}

// RFC 4493 §4 AES-128 test vectors.
func TestCMACVectors(t *testing.T) {
	key := unhex(t, "2b7e1516 28aed2a6 abf71588 09cf4f3c")
	msg := unhex(t, "6bc1bee2 2e409f96 e93d7e11 7393172a"+
		"ae2d8a57 1e03ac9c 9eb76fac 45af8e51"+
		"30c81c46 a35ce411 e5fbc119 1a0a52ef"+
		"f69f2445 df4f9b17 ad2b417b e66c3710")
	cases := []struct {
		name string
		n    int
		want string
	}{
		{"empty", 0, "bb1d6929 e9593728 7fa37d12 9b756746"},
		{"16 bytes", 16, "070a16b4 6b4d4144 f79bdd9d d04a287c"},
		{"40 bytes", 40, "dfa66747 de9ae630 30ca3261 1497c827"},
		{"64 bytes", 64, "51f0bebf 7e3b9d92 fc497417 79363cfe"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CMAC(key, msg[:c.n])
			if want := unhex(t, c.want); !bytes.Equal(got[:], want) {
				t.Fatalf("got %x want %x", got, want)
			}
		})
	}
}

func TestCMACPanicsOnBadKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on 8-byte key")
		}
	}()
	CMAC(make([]byte, 8), nil)
}

func testKey() Key {
	return Key{ID: 7, Secret: []byte{
		0x2b, 0x7e, 0x15, 0x16, 0x28, 0xae, 0xd2, 0xa6,
		0xab, 0xf7, 0x15, 0x88, 0x09, 0xcf, 0x4f, 0x3c,
	}}
}

func clientPacket() []byte {
	p := ntp.Packet{Version: 4, Mode: ntp.ModeClient, Poll: 6, Precision: -20, TransmitTime: 0xE6F2A1B880000000}
	return p.Marshal()
}

func extensionField(typ uint16, length int) []byte {
	b := make([]byte, length)
	binary.BigEndian.PutUint16(b[0:2], typ)
	binary.BigEndian.PutUint16(b[2:4], uint16(length))
	return b
}

func TestAppendDecodeVerify(t *testing.T) {
	k := testKey()
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"header only", clientPacket()},
		{"header + extension field", append(clientPacket(), extensionField(0x0104, 28)...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			wire := k.Append(c.body)
			if len(wire) != len(c.body)+ntp.MACSizeCMAC {
				t.Fatalf("wire length %d", len(wire))
			}
			if !bytes.Equal(wire[:len(c.body)], c.body) {
				t.Fatal("Append altered the packet body")
			}
			p, mac, off, err := ntp.Decode(wire)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if p.Mode != ntp.ModeClient || p.TransmitTime != 0xE6F2A1B880000000 {
				t.Fatal("header corrupted")
			}
			if mac == nil || mac.KeyID != 7 || len(mac.Digest) != 16 {
				t.Fatalf("mac %+v", mac)
			}
			if off != len(c.body) {
				t.Fatalf("mac offset %d want %d", off, len(c.body))
			}
			if !k.Verify(wire[:off], mac) {
				t.Fatal("Verify rejected a valid MAC")
			}
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	k := testKey()
	body := clientPacket()
	wire := k.Append(body)
	_, goodMAC, off, err := ntp.Decode(wire)
	if err != nil {
		t.Fatal(err)
	}

	tamperedHeader := append([]byte(nil), wire[:off]...)
	tamperedHeader[40] ^= 0x01 // transmit timestamp

	badDigest := &ntp.MAC{KeyID: goodMAC.KeyID, Digest: append([]byte(nil), goodMAC.Digest...)}
	badDigest.Digest[5] ^= 0x80

	cases := []struct {
		name string
		pkt  []byte
		mac  *ntp.MAC
	}{
		{"header tampered", tamperedHeader, goodMAC},
		{"digest tampered", wire[:off], badDigest},
		{"wrong key id", wire[:off], &ntp.MAC{KeyID: 8, Digest: goodMAC.Digest}},
		{"crypto-NAK", wire[:off], &ntp.MAC{}},
		{"nil mac", wire[:off], nil},
		{"short digest", wire[:off], &ntp.MAC{KeyID: 7, Digest: goodMAC.Digest[:15]}},
		{"long digest", wire[:off], &ntp.MAC{KeyID: 7, Digest: append(append([]byte(nil), goodMAC.Digest...), 0)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if k.Verify(c.pkt, c.mac) {
				t.Fatal("Verify accepted an invalid MAC")
			}
		})
	}
	// Sanity: the untampered case still verifies with the other key id ruled out.
	if !k.Verify(wire[:off], goodMAC) {
		t.Fatal("control case failed")
	}
	if (Key{ID: 8, Secret: k.Secret}).Verify(wire[:off], goodMAC) {
		t.Fatal("a ring entry with a different id must not verify")
	}
}

func TestParseKeys(t *testing.T) {
	const good = `# carillon keys
1 AES128CMAC 2b7e151628aed2a6abf7158809cf4f3c
  # indented comment

42 aes128cmac 000102030405060708090a0b0c0d0e0f   # trailing comment
65535	Aes128Cmac	ffffffffffffffffffffffffffffffff
`
	keys, err := ParseKeys(strings.NewReader(good))
	if err != nil {
		t.Fatalf("ParseKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("got %d keys", len(keys))
	}
	if k := keys[1]; k.ID != 1 || !bytes.Equal(k.Secret, testKey().Secret) {
		t.Fatalf("key 1: %+v", k)
	}
	if k := keys[42]; k.ID != 42 || k.Secret[15] != 0x0f {
		t.Fatalf("key 42: %+v", k)
	}
	if _, ok := keys[65535]; !ok {
		t.Fatal("key 65535 missing")
	}

	empty, err := ParseKeys(strings.NewReader("# nothing here\n\n"))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty ring: %v %v", empty, err)
	}

	bad := []struct {
		name    string
		input   string
		wantSub string
	}{
		{"bad hex", "1 AES128CMAC zz7e151628aed2a6abf7158809cf4f3c\n", "not valid hex"},
		{"wrong length", "1 AES128CMAC 2b7e1516\n", "32 hex digits"},
		{"md5 unsupported", "1 MD5 2b7e151628aed2a6abf7158809cf4f3c\n", "MD5"},
		{"sha1 unsupported", "1 SHA1 2b7e151628aed2a6abf7158809cf4f3c\n", "only AES128CMAC"},
		{"duplicate id", "1 AES128CMAC 2b7e151628aed2a6abf7158809cf4f3c\n1 AES128CMAC 000102030405060708090a0b0c0d0e0f\n", "duplicate"},
		{"id 0", "0 AES128CMAC 2b7e151628aed2a6abf7158809cf4f3c\n", "out of range"},
		{"id 70000", "70000 AES128CMAC 2b7e151628aed2a6abf7158809cf4f3c\n", "out of range"},
		{"id not numeric", "one AES128CMAC 2b7e151628aed2a6abf7158809cf4f3c\n", "not a decimal integer"},
		{"too few fields", "1 AES128CMAC\n", "expected"},
		{"too many fields", "1 AES128CMAC 2b7e151628aed2a6abf7158809cf4f3c extra\n", "expected"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseKeys(strings.NewReader(c.input))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not mention %q", err, c.wantSub)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Fatalf("error %q lacks a line number", err)
			}
		})
	}
}

func TestLoadKeysPermissions(t *testing.T) {
	dir := t.TempDir()
	content := []byte("1 AES128CMAC 2b7e151628aed2a6abf7158809cf4f3c\n")

	secure := filepath.Join(dir, "keys-0600")
	if err := os.WriteFile(secure, content, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeys(secure)
	if err != nil {
		t.Fatalf("LoadKeys 0600: %v", err)
	}
	if len(keys) != 1 || keys[1].ID != 1 {
		t.Fatalf("keys: %+v", keys)
	}

	open := filepath.Join(dir, "keys-0644")
	if err := os.WriteFile(open, content, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadKeys(open)
	if err == nil {
		t.Fatal("expected error for 0644 keys file")
	}
	if !strings.Contains(err.Error(), "permissions") || !strings.Contains(err.Error(), open) {
		t.Fatalf("error %q should mention permissions and the path", err)
	}

	_, err = LoadKeys(filepath.Join(dir, "missing"))
	if err == nil {
		t.Fatal("expected error for a missing file")
	}
}
