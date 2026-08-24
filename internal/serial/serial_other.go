//go:build !linux && !freebsd

package serial

import (
	"errors"
	"fmt"
)

// ErrUnsupportedPlatform is returned on development hosts without a serial
// PPS backend.
var ErrUnsupportedPlatform = errors.New("serial PPS is unsupported on this platform")

func OpenCLOCAL(path string) (*Port, error) {
	return nil, fmt.Errorf("serial: open %s: %w", path, ErrUnsupportedPlatform)
}

func AttachPPS(path string) (*Port, string, error) {
	return nil, "", fmt.Errorf("serial: attach PPS to %s: %w", path, ErrUnsupportedPlatform)
}

func (p *Port) Close() error { return nil }
