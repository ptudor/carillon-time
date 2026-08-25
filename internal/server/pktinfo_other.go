//go:build !linux && !freebsd

package server

import (
	"net/netip"
	"syscall"
)

func enablePacketInfo(syscall.RawConn, string) error { return nil }

func destination([]byte, string) (netip.Addr, []byte) { return netip.Addr{}, nil }
