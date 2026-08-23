package auth

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Keys is the loaded key ring, indexed by id.
type Keys map[uint32]Key

// keyType is the only key type this daemon accepts, in the spelling ntpd
// and chrony use in their key files.
const keyType = "AES128CMAC"

// LoadKeys reads an ntpd-style keys file. It refuses a file that is readable
// by group or others, because the file holds shared secrets.
func LoadKeys(path string) (Keys, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("auth: open keys file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("auth: stat keys file %s: %w", path, err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, fmt.Errorf("auth: keys file %s has permissions %04o; it must not be readable by group or others (chmod 0600)", path, mode)
	}

	keys, err := ParseKeys(f)
	if err != nil {
		return nil, fmt.Errorf("auth: keys file %s: %w", path, err)
	}
	return keys, nil
}

// ParseKeys parses the keys file syntax from r: one `<id> <type> <key>` per
// line, blank lines and `#` comments ignored. The id is 1..65535, the type
// must be AES128CMAC (case-insensitive), and the key is exactly 32 hex
// digits. Duplicate ids are an error. An empty file is a valid, empty ring.
func ParseKeys(r io.Reader) (Keys, error) {
	keys := make(Keys)
	sc := bufio.NewScanner(r)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: expected `<id> <type> <key>`, got %d fields", lineNo, len(fields))
		}

		id, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("line %d: key id %q is not a decimal integer", lineNo, fields[0])
		}
		if id < 1 || id > 65535 {
			return nil, fmt.Errorf("line %d: key id %d out of range 1..65535", lineNo, id)
		}
		if _, dup := keys[uint32(id)]; dup {
			return nil, fmt.Errorf("line %d: duplicate key id %d", lineNo, id)
		}

		if !strings.EqualFold(fields[1], keyType) {
			return nil, fmt.Errorf("line %d: key type %q is not supported; only %s is (MD5 and SHA-1 MACs are not implemented)", lineNo, fields[1], keyType)
		}

		if len(fields[2]) != 2*KeySize {
			return nil, fmt.Errorf("line %d: key must be exactly %d hex digits (%d bytes), got %d characters", lineNo, 2*KeySize, KeySize, len(fields[2]))
		}
		secret, err := hex.DecodeString(fields[2])
		if err != nil {
			return nil, fmt.Errorf("line %d: key is not valid hex: %w", lineNo, err)
		}

		keys[uint32(id)] = Key{ID: uint32(id), Secret: secret}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return keys, nil
}
