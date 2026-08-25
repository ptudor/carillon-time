//go:build !linux && !freebsd && !darwin

package server

import "syscall"

// receiveBufferSize is unavailable on this platform; zero means "unknown".
func receiveBufferSize(syscall.RawConn) (int, error) { return 0, nil }
