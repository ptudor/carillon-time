// Package sockts obtains kernel receive timestamps for UDP datagrams. A
// timestamp taken by the kernel when the packet arrived is free of the
// daemon's own scheduling latency, which is the largest avoidable error in
// an NTP exchange on the client side.
//
// Enable turns the feature on for a socket; Parse extracts the timestamp from
// the out-of-band data that ReadMsgUDPAddrPort returns. On platforms without
// a backend, Enable is a no-op and Parse never finds a timestamp, and callers
// fall back to reading the clock themselves.
package sockts

// OOBSize is a control-message buffer size sufficient for Parse on every
// supported platform (a timespec plus its cmsg header is under 40 bytes).
const OOBSize = 128
