// Package auth implements NTP message authentication with AES-128-CMAC
// (RFC 8573), the CMAC algorithm itself (RFC 4493, which the standard library
// does not provide), and the ntpd-style keys file that holds the shared
// secrets. MD5 and SHA-1 MACs are deliberately not supported.
package auth

import (
	"crypto/aes"
	"crypto/subtle"
	"encoding/binary"
	"fmt"

	"carillon/internal/ntp"
)

// KeySize is the AES-128 key length in bytes.
const KeySize = 16

// cmacRb is the constant used in CMAC subkey generation for a 128-bit block
// (RFC 4493 §2.3).
const cmacRb = 0x87

// CMAC computes AES-CMAC (RFC 4493) of msg under a 16-byte key. It panics on
// a wrong key length: keys are validated when the ring is loaded, so a bad
// length here is a programming error, not an input error.
func CMAC(key, msg []byte) [16]byte {
	if len(key) != KeySize {
		panic(fmt.Sprintf("auth: CMAC key must be %d bytes, got %d", KeySize, len(key)))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		// Unreachable for a 16-byte key; aes.NewCipher only rejects sizes.
		panic("auth: aes.NewCipher: " + err.Error())
	}

	// Subkeys K1 and K2 (RFC 4493 §2.3).
	var l [16]byte
	block.Encrypt(l[:], l[:])
	k1 := shiftLeftAndCondition(l)
	k2 := shiftLeftAndCondition(k1)

	// Split the message into 16-byte blocks; the last block is padded and
	// XORed with K2 if incomplete, or XORed with K1 if complete. An empty
	// message counts as one incomplete block (§2.4).
	n := (len(msg) + 15) / 16
	complete := n != 0 && len(msg)%16 == 0
	if n == 0 {
		n = 1
	}

	var x, y, last [16]byte
	if complete {
		copy(last[:], msg[(n-1)*16:])
		xorInto(&last, k1)
	} else {
		copy(last[:], msg[(n-1)*16:])
		last[len(msg)-(n-1)*16] = 0x80
		xorInto(&last, k2)
	}

	for i := 0; i < n-1; i++ {
		copy(y[:], msg[i*16:(i+1)*16])
		xorInto(&y, x)
		block.Encrypt(x[:], y[:])
	}
	y = last
	xorInto(&y, x)
	block.Encrypt(x[:], y[:])
	return x
}

// shiftLeftAndCondition returns (in << 1) XOR Rb-if-MSB-was-set, the subkey
// derivation step of RFC 4493 §2.3.
func shiftLeftAndCondition(in [16]byte) [16]byte {
	var out [16]byte
	var carry byte
	for i := 15; i >= 0; i-- {
		out[i] = in[i]<<1 | carry
		carry = in[i] >> 7
	}
	if in[0]&0x80 != 0 {
		out[15] ^= cmacRb
	}
	return out
}

func xorInto(dst *[16]byte, src [16]byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}

// Key is one entry of the key ring.
type Key struct {
	ID     uint32
	Secret []byte // 16 bytes, AES-128
}

// Append appends the RFC 8573 MAC trailer — a 4-byte big-endian key id
// followed by the 16-byte CMAC over pkt — to pkt and returns the result.
func (k Key) Append(pkt []byte) []byte {
	digest := CMAC(k.Secret, pkt)
	out := make([]byte, 0, len(pkt)+ntp.MACSizeCMAC)
	out = append(out, pkt...)
	out = binary.BigEndian.AppendUint32(out, k.ID)
	return append(out, digest[:]...)
}

// Verify reports whether mac authenticates pkt, the bytes preceding the MAC
// (b[:macOffset] as returned by ntp.Decode). The digest comparison is
// constant-time. It returns false for a nil mac, a crypto-NAK, a key id
// other than k's, or a digest that is not 16 bytes.
func (k Key) Verify(pkt []byte, mac *ntp.MAC) bool {
	if mac == nil || mac.IsCryptoNAK() || mac.KeyID != k.ID || len(mac.Digest) != 16 {
		return false
	}
	want := CMAC(k.Secret, pkt)
	return subtle.ConstantTimeCompare(want[:], mac.Digest) == 1
}
