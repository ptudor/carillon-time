//go:build !linux && !freebsd

package server

import (
	"net/netip"
	"syscall"
)

func enablePacketInfo(syscall.RawConn, string) error { return nil }

func martianReceiveFlags(int) bool { return false }

func destination([]byte, string) (netip.Addr, []byte, bool) { return netip.Addr{}, nil, false }
