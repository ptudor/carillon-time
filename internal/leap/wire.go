package leap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	ExtensionType uint16 = 0xf504
	WireVersion          = 1
	FieldSize            = 88
	ChunkSize            = 256
	MaxFieldSize         = FieldSize + ChunkSize
	Probe         uint8  = 1
	ManifestReply uint8  = 2
	Get           uint8  = 3
	Data          uint8  = 4
	OK            uint16 = 0
	NoTable       uint16 = 1
	NotFound      uint16 = 2
)

var ErrUnsupported = errors.New("leap: unsupported extension")

type Message struct {
	Operation uint8
	Result    uint16
	ID        [16]byte
	Manifest  Manifest
	Offset    uint32
	Count     uint16
	Data      []byte
}

func (m Message) validate() error {
	bad := func() error { return Reject("framing", "invalid CLPS operation, length, padding or metadata") }
	if m.ID == ([16]byte{}) {
		return bad()
	}
	if m.Manifest != (Manifest{}) {
		if err := m.Manifest.Validate(); err != nil {
			return bad()
		}
	}
	switch m.Operation {
	case Probe:
		if m.Result != OK || m.Offset != 0 || m.Count != 0 || len(m.Data) != 0 {
			return bad()
		}
	case ManifestReply:
		if m.Offset != 0 || m.Count != 0 || len(m.Data) != 0 ||
			(m.Result != OK && m.Result != NoTable) ||
			(m.Result == NoTable) != (m.Manifest == (Manifest{})) {
			return bad()
		}
	case Get:
		if m.Result != OK || m.Manifest == (Manifest{}) || m.Offset >= m.Manifest.Size || m.Offset%ChunkSize != 0 || m.Count != ChunkSize || len(m.Data) != 0 {
			return bad()
		}
	case Data:
		if m.Manifest == (Manifest{}) || m.Offset >= m.Manifest.Size || m.Offset%ChunkSize != 0 {
			return bad()
		}
		if m.Result == NotFound {
			if m.Count != 0 || len(m.Data) != 0 {
				return bad()
			}
		} else if m.Result != OK || int(m.Count) != len(m.Data) || uint32(m.Count) != min(uint32(ChunkSize), m.Manifest.Size-m.Offset) {
			return bad()
		}
	default:
		return bad()
	}
	return nil
}

// AppendTo adds exactly one framed field. GET has authenticated zero padding
// reserving enough response space for a complete chunk.
func (m Message) AppendTo(dst []byte) ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	length := FieldSize + ((len(m.Data) + 3) &^ 3)
	if m.Operation == Get {
		length = MaxFieldSize
	}
	start := len(dst)
	dst = append(dst, make([]byte, length)...)
	b := dst[start:]
	binary.BigEndian.PutUint16(b, ExtensionType)
	binary.BigEndian.PutUint16(b[2:], uint16(length))
	copy(b[4:8], "CLPS")
	b[8], b[9] = WireVersion, m.Operation
	binary.BigEndian.PutUint16(b[10:], m.Result)
	copy(b[12:28], m.ID[:])
	binary.BigEndian.PutUint64(b[28:], m.Manifest.Updated)
	binary.BigEndian.PutUint64(b[36:], m.Manifest.Expires)
	copy(b[44:76], m.Manifest.Digest[:])
	binary.BigEndian.PutUint32(b[76:], m.Manifest.Size)
	binary.BigEndian.PutUint32(b[80:], m.Offset)
	binary.BigEndian.PutUint16(b[84:], m.Count)
	copy(b[88:], m.Data)
	return dst, nil
}

// DecodeFields scans only the extension region located by ntp.Decode. Call
// after MAC verification. Unknown field types are skipped without allocation.
func DecodeFields(fields []byte) (*Message, error) {
	var found *Message
	seen, unsupported := false, false
	for len(fields) != 0 {
		if len(fields) < 4 {
			return nil, Reject("framing", "short extension header")
		}
		n := int(binary.BigEndian.Uint16(fields[2:4]))
		if n < 16 || n%4 != 0 || n > len(fields) {
			return nil, Reject("framing", "bad extension size")
		}
		b := fields[:n]
		fields = fields[n:]
		if binary.BigEndian.Uint16(b) != ExtensionType {
			continue
		}
		if seen {
			return nil, Reject("framing", "duplicate CLPS fields")
		}
		seen = true
		if n < FieldSize || n > MaxFieldSize {
			return nil, Reject("framing", "bad CLPS size")
		}
		if string(b[4:8]) != "CLPS" || b[8] != WireVersion {
			unsupported = true
			continue
		}
		m := &Message{Operation: b[9], Result: binary.BigEndian.Uint16(b[10:12])}
		copy(m.ID[:], b[12:28])
		m.Manifest.Updated = binary.BigEndian.Uint64(b[28:36])
		m.Manifest.Expires = binary.BigEndian.Uint64(b[36:44])
		copy(m.Manifest.Digest[:], b[44:76])
		m.Manifest.Size = binary.BigEndian.Uint32(b[76:80])
		m.Offset = binary.BigEndian.Uint32(b[80:84])
		m.Count = binary.BigEndian.Uint16(b[84:86])
		if binary.BigEndian.Uint16(b[86:88]) != 0 {
			return nil, Reject("framing", "reserved data is nonzero")
		}
		dataLen := 0
		expected := FieldSize
		if m.Operation == Data {
			dataLen = int(m.Count)
			expected += (dataLen + 3) &^ 3
		}
		if m.Operation == Get {
			expected = MaxFieldSize
		}
		if expected != n || dataLen > n-FieldSize {
			return nil, Reject("framing", "chunk length disagrees with field")
		}
		m.Data = b[FieldSize : FieldSize+dataLen]
		if !allZero(b[FieldSize+dataLen:]) {
			return nil, Reject("framing", "padding is nonzero")
		}
		if err := m.validate(); err != nil {
			return nil, err
		}
		found = m
	}
	if unsupported {
		return nil, ErrUnsupported
	}
	return found, nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// Reply returns a stateless response. The caller has already authorized and
// authenticated the requester and checked the object's current validity.
func Reply(req *Message, object *Object) (Message, error) {
	if req == nil {
		return Message{}, errors.New("leap: missing request")
	}
	if err := req.validate(); err != nil {
		return Message{}, err
	}
	r := Message{ID: req.ID}
	switch req.Operation {
	case Probe:
		r.Operation = ManifestReply
		if object == nil {
			r.Result = NoTable
		} else {
			r.Manifest = object.manifest
		}
	case Get:
		r.Operation, r.Manifest, r.Offset = Data, req.Manifest, req.Offset
		if object == nil || object.manifest != req.Manifest {
			r.Result = NotFound
		} else {
			r.Count = uint16(min(uint32(ChunkSize), r.Manifest.Size-r.Offset))
			r.Data = bytes.Clone(object.data[r.Offset : r.Offset+uint32(r.Count)])
		}
	default:
		return r, fmt.Errorf("leap: operation %d is not a request", req.Operation)
	}
	return r, nil
}
