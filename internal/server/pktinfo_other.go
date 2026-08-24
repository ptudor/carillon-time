//go:build !linux && !freebsd

package server

import "syscall"

func enablePacketInfo(syscall.RawConn, string) error { return nil }

func sourceControl([]byte, string) []byte { return nil }
