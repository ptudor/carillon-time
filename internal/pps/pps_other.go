//go:build !linux && !freebsd

package pps

import "fmt"

func Open(path string, edge Edge) (Reader, error) {
	return nil, fmt.Errorf("pps: open %s: %w", path, ErrUnsupportedPlatform)
}
