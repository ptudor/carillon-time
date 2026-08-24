//go:build !linux && !freebsd

package serial

import (
	"errors"
	"fmt"
	"time"
)

// ErrUnsupportedPlatform is returned on development hosts without a serial
// PPS backend.
var ErrUnsupportedPlatform = errors.New("serial PPS is unsupported on this platform")

func OpenCLOCAL(path string) (*Port, error) {
	return nil, fmt.Errorf("serial: open %s: %w", path, ErrUnsupportedPlatform)
}

func OpenNMEA(path string, baud int) (*Port, error) {
	return nil, fmt.Errorf("serial: open NMEA %s at %d baud: %w", path, baud, ErrUnsupportedPlatform)
}

func AttachPPS(path string) (*Port, string, error) {
	return nil, "", fmt.Errorf("serial: attach PPS to %s: %w", path, ErrUnsupportedPlatform)
}

func (p *Port) Close() error { return nil }

func (p *Port) ReadTimeout([]byte, time.Duration) (int, error) {
	return 0, ErrUnsupportedPlatform
}
