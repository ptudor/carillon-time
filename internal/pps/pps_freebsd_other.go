//go:build freebsd && !amd64 && !arm64

package pps

import "fmt"

func Open(path string, edge Edge) (Reader, error) {
	return nil, fmt.Errorf("pps: open %s: %w on this FreeBSD architecture", path, ErrUnsupportedPlatform)
}
