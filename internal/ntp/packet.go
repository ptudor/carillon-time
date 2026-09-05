package ntp

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// Version is the protocol version this daemon speaks.
	Version = 4

	// HeaderSize is the size of the fixed NTP packet header.
	HeaderSize = 48

	// MaxPacketSize is the largest datagram the daemon will read. Anything
	// bigger is not a time packet we understand.
	MaxPacketSize = 1500

	// MACSizeCMAC is the size of the AES-128-CMAC trailer: 4-byte key id and
	// 16-byte digest (RFC 8573).
	MACSizeCMAC = 4 + 16

	// macSizeSHA1 is the size of a legacy SHA-1 trailer. Recognised by the
	// framing so such packets decode, never verified.
	macSizeSHA1 = 4 + 20

	// CryptoNAKSize is a MAC field consisting of a zero key id only, which a
	// server sends when it cannot authenticate a request.
	CryptoNAKSize = cryptoNAKSize

	// cryptoNAKSize is a MAC field consisting of a zero key id only, which a
	// server sends to say it could not authenticate the request.
	cryptoNAKSize = 4

	// minExtensionField is the minimum length of an extension field that is
	// not the last one in the packet (RFC 7822 §3).
	minExtensionField = 16
)

// Errors returned by Decode.
var (
	ErrShort   = errors.New("ntp: packet shorter than 48 bytes")
	ErrVersion = errors.New("ntp: unsupported protocol version")
	ErrTrailer = errors.New("ntp: malformed extension field or MAC trailer")
)

// Packet is the decoded 48-byte NTP header.
type Packet struct {
	Leap           Leap
	Version        uint8
	Mode           Mode
	Stratum        uint8
	Poll           int8
	Precision      int8
	RootDelay      Short
	RootDispersion Short
	ReferenceID    RefID
	ReferenceTime  Time
	OriginTime     Time
	ReceiveTime    Time
	TransmitTime   Time
}

// MAC is the message authentication trailer. A crypto-NAK (a server telling
// the client it could not authenticate the request) decodes as KeyID 0 with a
// nil Digest.
type MAC struct {
	KeyID  uint32
	Digest []byte
}

// IsCryptoNAK reports whether the trailer is a crypto-NAK.
func (m *MAC) IsCryptoNAK() bool { return m != nil && m.KeyID == 0 && len(m.Digest) == 0 }

// IsKiss reports whether the packet is a kiss-o'-death (stratum 0) packet.
func (p *Packet) IsKiss() bool { return p.Stratum == 0 }

// KissCode returns the kiss code text for a kiss packet, "" otherwise.
func (p *Packet) KissCode() string {
	if !p.IsKiss() || !p.ReferenceID.IsText() {
		return ""
	}
	return p.ReferenceID.Text()
}

// Decode parses the fixed header and locates a trailing MAC, skipping any
// extension fields (RFC 7822 framing). It returns the byte offset at which
// the MAC begins — the region [0, macOffset) is what a MAC covers — or
// len(b) when there is no MAC. Version 0 and versions above 4 are rejected;
// versions 1–3 are accepted so old clients can be answered.
func Decode(b []byte) (p Packet, mac *MAC, macOffset int, err error) {
	if len(b) < HeaderSize {
		return p, nil, 0, ErrShort
	}
	p.Leap = Leap(b[0] >> 6)
	p.Version = (b[0] >> 3) & 0x7
	p.Mode = Mode(b[0] & 0x7)
	if p.Version == 0 || p.Version > Version {
		return p, nil, 0, fmt.Errorf("%w: %d", ErrVersion, p.Version)
	}
	p.Stratum = b[1]
	p.Poll = int8(b[2])
	p.Precision = int8(b[3])
	p.RootDelay = Short(binary.BigEndian.Uint32(b[4:8]))
	p.RootDispersion = Short(binary.BigEndian.Uint32(b[8:12]))
	copy(p.ReferenceID[:], b[12:16])
	p.ReferenceTime = Time(binary.BigEndian.Uint64(b[16:24]))
	p.OriginTime = Time(binary.BigEndian.Uint64(b[24:32]))
	p.ReceiveTime = Time(binary.BigEndian.Uint64(b[32:40]))
	p.TransmitTime = Time(binary.BigEndian.Uint64(b[40:48]))

	// Anything after the header is extension fields followed by an optional
	// MAC. An extension field is at least 16 bytes and a MAC is at most 24,
	// so while more than 24 bytes remain we must be looking at an extension
	// field; its length field tells us how far to skip.
	off := HeaderSize
	for len(b)-off > macSizeSHA1 {
		if len(b)-off < 4 {
			return p, nil, 0, ErrTrailer
		}
		l := int(binary.BigEndian.Uint16(b[off+2 : off+4]))
		if l < minExtensionField || l%4 != 0 || off+l > len(b) {
			return p, nil, 0, ErrTrailer
		}
		off += l
	}
	switch rest := len(b) - off; rest {
	case 0:
		return p, nil, len(b), nil
	case cryptoNAKSize:
		if binary.BigEndian.Uint32(b[off:]) != 0 {
			return p, nil, 0, ErrTrailer
		}
		return p, &MAC{}, off, nil
	case MACSizeCMAC, macSizeSHA1:
		m := &MAC{KeyID: binary.BigEndian.Uint32(b[off:]), Digest: b[off+4:]}
		return p, m, off, nil
	default:
		return p, nil, 0, ErrTrailer
	}
}

// AppendTo appends the 48-byte header encoding of p to dst.
func (p *Packet) AppendTo(dst []byte) []byte {
	var h [HeaderSize]byte
	h[0] = byte(p.Leap)<<6 | (p.Version&0x7)<<3 | byte(p.Mode)&0x7
	h[1] = p.Stratum
	h[2] = byte(p.Poll)
	h[3] = byte(p.Precision)
	binary.BigEndian.PutUint32(h[4:8], uint32(p.RootDelay))
	binary.BigEndian.PutUint32(h[8:12], uint32(p.RootDispersion))
	copy(h[12:16], p.ReferenceID[:])
	binary.BigEndian.PutUint64(h[16:24], uint64(p.ReferenceTime))
	binary.BigEndian.PutUint64(h[24:32], uint64(p.OriginTime))
	binary.BigEndian.PutUint64(h[32:40], uint64(p.ReceiveTime))
	binary.BigEndian.PutUint64(h[40:48], uint64(p.TransmitTime))
	return append(dst, h[:]...)
}

// Marshal returns the 48-byte header encoding of p.
func (p *Packet) Marshal() []byte { return p.AppendTo(make([]byte, 0, HeaderSize)) }
