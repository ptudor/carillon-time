package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"syscall"
	"time"

	"carillon/internal/ntp"
	"carillon/internal/sockts"
)

const serverOOBSize = 256

// minRecvBuffer is the floor the halving retry in setReadBuffer stops at. It
// mirrors config.MinRecvBuffer, which config validates recv_buffer against;
// config imports this package, so the constant cannot be imported back.
const minRecvBuffer = 64 << 10

// RecvBufferSysctl names the kernel knob that caps SO_RCVBUF on this OS, for
// error messages and for -check to point at.
func RecvBufferSysctl() string {
	switch runtime.GOOS {
	case "linux":
		return "net.core.rmem_max"
	case "freebsd":
		return "kern.ipc.maxsockbuf"
	default:
		return "the kernel socket-buffer limit"
	}
}

// ServiceConfig configures a set of NTP UDP listeners.
type ServiceConfig struct {
	Listen  []netip.AddrPort
	Handler Config
	Log     *slog.Logger

	// RecvBuffer is the SO_RCVBUF size in bytes requested for each socket.
	// Zero leaves the kernel default. The effective size is logged, because
	// the kernel clamps the request to its own maximum and a busy public
	// server that silently gets the default drops bursts with no other
	// evidence than clients that were never answered.
	RecvBuffer int
}

// Service owns all configured UDP sockets. They are opened by Listen before
// the engine starts so bind and socket-option failures are startup failures.
type Service struct {
	listeners []*udpListener
	closeOnce sync.Once
}

type udpListener struct {
	addr       netip.AddrPort
	network    string
	conn       *net.UDPConn
	handler    *Handler
	log        *slog.Logger
	recvBuffer int

	// overflow is the last value of the kernel's cumulative drop counter for
	// this socket, owned by the serve goroutine.
	overflow     uint32
	haveOverflow bool

	// broadcasts recognises this host's directed broadcast addresses, which
	// cannot be told from ordinary unicast addresses without the
	// interfaces' own configuration.
	broadcasts *localBroadcasts
}

// Listen opens every configured socket and prepares its receive timestamp and
// destination-address control messages. An error closes sockets already open.
func Listen(cfg ServiceConfig) (*Service, error) {
	if len(cfg.Listen) == 0 {
		return nil, errors.New("server: no listen addresses")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	s := &Service{}
	for _, addr := range cfg.Listen {
		l, err := listenOne(addr, cfg.Handler, cfg.RecvBuffer, cfg.Log)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.listeners = append(s.listeners, l)
	}
	return s, nil
}

func listenOne(addr netip.AddrPort, hcfg Config, recvBuffer int, log *slog.Logger) (*udpListener, error) {
	network := "udp6"
	if addr.Addr().Unmap().Is4() {
		network = "udp4"
		addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	}
	conn, err := net.ListenUDP(network, net.UDPAddrFromAddrPort(addr))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("server: listen %s: UDP address is in use (another time daemon such as ntpd, chronyd, or systemd-timesyncd may be running): %w", addr, err)
		}
		return nil, fmt.Errorf("server: listen %s: %w", addr, err)
	}
	fail := func(format string, args ...any) (*udpListener, error) {
		_ = conn.Close()
		return nil, fmt.Errorf(format, args...)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return fail("server: listen %s: socket access: %w", addr, err)
	}
	if err := sockts.Enable(raw); err != nil {
		return fail("server: listen %s: receive timestamps: %w", addr, err)
	}
	if err := enablePacketInfo(raw, network); err != nil {
		return fail("server: listen %s: destination address capture: %w", addr, err)
	}
	if err := enableOverflowReporting(raw); err != nil {
		// A diagnostic, not a requirement: keep serving time and say which
		// metric will stay at zero.
		log.Warn("kernel receive-overflow reporting unavailable; carillon_server_kernel_drops_total will not count",
			"listen", addr, "error", err)
	}
	if recvBuffer > 0 {
		if err := setReadBuffer(conn, recvBuffer, addr, log); err != nil {
			return fail("%w", err)
		}
	}
	effective, err := receiveBufferSize(raw)
	if err != nil {
		return fail("server: listen %s: %w", addr, err)
	}
	h, err := NewHandler(hcfg)
	if err != nil {
		return fail("server: listen %s: %w", addr, err)
	}
	actual := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	l := &udpListener{
		addr: actual, network: network, conn: conn, handler: h,
		log: log.With("listen", actual), broadcasts: newLocalBroadcasts(),
	}
	l.recvBuffer = effective
	return l, nil
}

// setReadBuffer asks for want bytes of SO_RCVBUF, halving the request until
// the kernel accepts it.
//
// Linux clamps SO_RCVBUF to net.core.rmem_max silently, so the request always
// succeeds and the granted size is read back afterwards. FreeBSD does not
// clamp: sbreserve_locked returns 0 when the request exceeds sb_max_adj
// (kern.ipc.maxsockbuf, default 2 MB adjusted to about 1.86 MB) and
// setsockopt fails with ENOBUFS. The example configuration's public block
// asks for 4 MB, so copying it to a FreeBSD host with default sysctls used to
// stop the daemon starting. Serving time with a smaller buffer and a warning
// naming the sysctl to raise is better than not serving time.
// bufferSetter is the seam setReadBuffer works through, satisfied by
// *net.UDPConn and by a test's fake kernel.
type bufferSetter interface {
	SetReadBuffer(bytes int) error
}

func setReadBuffer(conn bufferSetter, want int, addr netip.AddrPort, log *slog.Logger) error {
	var lastErr error
	var tried []int
	// Halving an arbitrary request can step straight past the documented
	// minimum without ever asking for it: 100000 halves to 50000, below the
	// 65536 floor, and startup then failed with an error claiming the
	// minimum had been tried (RA6X-043). The floor is therefore always the
	// last attempt, exactly once.
	for size := want; ; size /= 2 {
		if size < minRecvBuffer {
			size = minRecvBuffer
		}
		tried = append(tried, size)
		err := conn.SetReadBuffer(size)
		if err == nil {
			if size != want {
				log.Warn("receive buffer request was refused; using a smaller one",
					"listen", addr, "requested", want, "granted_request", size,
					"attempted", tried,
					"hint", RecvBufferSysctl()+" must be raised before a larger buffer can be granted",
					"error", lastErr)
			}
			return nil
		}
		lastErr = err
		// Only a capacity refusal is worth retrying smaller; anything else
		// will fail identically at every size.
		if !errors.Is(err, syscall.ENOBUFS) && !errors.Is(err, syscall.EINVAL) {
			break
		}
		if size == minRecvBuffer {
			break
		}
	}
	return fmt.Errorf("server: listen %s: receive buffer of %d bytes (attempted %v, down to %d): raise %s: %w",
		addr, want, tried, minRecvBuffer, RecvBufferSysctl(), lastErr)
}

// ReceiveBuffers returns each listener's effective SO_RCVBUF in bytes, in the
// same order as Addrs.
func (s *Service) ReceiveBuffers() []int {
	out := make([]int, 0, len(s.listeners))
	for _, l := range s.listeners {
		out = append(out, l.recvBuffer)
	}
	return out
}

// Addrs returns the bound addresses. It exposes kernel-selected ephemeral
// ports to tests and diagnostic callers.
func (s *Service) Addrs() []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(s.listeners))
	for _, l := range s.listeners {
		out = append(out, l.addr)
	}
	return out
}

// Close closes every UDP socket. It is safe to call more than once.
func (s *Service) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		for _, l := range s.listeners {
			_ = l.conn.Close()
		}
	})
}

// Serve runs one owning goroutine per socket until ctx is cancelled or a
// listener fails. A failure closes its siblings and is returned to the caller.
func (s *Service) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, len(s.listeners))
	for _, l := range s.listeners {
		go func() { errCh <- l.serve(ctx) }()
	}
	stopClose := context.AfterFunc(ctx, s.Close)
	defer stopClose()

	var first error
	for range s.listeners {
		if err := <-errCh; err != nil && first == nil {
			first = err
			cancel()
			s.Close()
		}
	}
	return first
}

func (l *udpListener) serve(ctx context.Context) error {
	buf := make([]byte, ntp.MaxPacketSize+1)
	oob := make([]byte, serverOOBSize)
	missingTimestampWarned := false
	broadcastWarned := false
	for {
		n, oobn, flags, from, err := l.conn.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Temporary() {
				l.log.Debug("temporary receive error", "error", err)
				continue
			}
			return fmt.Errorf("server: receive on %s: %w", l.addr, err)
		}
		c := l.handler.stats.family(from.Addr())
		l.recordOverflow(c, oob[:oobn])
		if flags&syscall.MSG_TRUNC != 0 || n > ntp.MaxPacketSize {
			c.oversize.Add(1)
			continue
		}
		// FreeBSD reports a broadcast or multicast delivery in the flags
		// word. Answering one would put an illegal source address on the
		// reply — which FreeBSD accepts — and turn a single forged datagram
		// into a reply from every server on the segment.
		if martianReceiveFlags(flags) {
			c.martian.Add(1)
			continue
		}
		received := l.handler.now()
		if ts, ok := sockts.Parse(oob[:oobn]); ok {
			received = ts
		} else {
			c.missingKernelTS.Add(1)
			if !missingTimestampWarned {
				l.log.Warn("kernel receive timestamp missing; using a user-space timestamp")
				missingTimestampWarned = true
			}
		}
		// A request addressed to a broadcast or multicast group would need an
		// illegal source address on the reply, and one such datagram would ask
		// every host on the subnet to answer at once.
		dst, replyOOB, martian := destination(oob[:oobn], l.network)
		if martian || martianDestination(dst) {
			c.martian.Add(1)
			continue
		}
		// A *directed* broadcast — 192.0.2.255 on a /24 — cannot be
		// recognised from the address alone; it depends on the interface's
		// own configuration. Linux reports the delivery in the flags word
		// above, FreeBSD does not, so both are covered by comparing the
		// destination against this host's broadcast addresses (RA6X-026).
		// Exactly one martian outcome is counted for any one datagram.
		if bcast, known := l.broadcasts.isBroadcast(dst, time.Now()); bcast {
			c.martian.Add(1)
			continue
		} else if !known && !broadcastWarned {
			l.log.Warn("cannot read the local interface addresses; directed broadcasts cannot be recognised")
			broadcastWarned = true
		}
		response := l.handler.Handle(buf[:n], from, received, time.Now())
		if response == nil {
			continue
		}
		if _, _, err := l.conn.WriteMsgUDPAddrPort(response, replyOOB, from); err != nil {
			l.log.Debug("send failed", "client", from, "error", err)
		}
	}
}

// recordOverflow folds the kernel's cumulative socket drop counter into the
// stats as a delta. The kernel counter is 32 bits and wraps, which unsigned
// subtraction handles; the first reading is taken whole, because anything it
// already counted was dropped by this socket.
func (l *udpListener) recordOverflow(c *counters, oob []byte) {
	got, ok := parseOverflow(oob)
	if !ok {
		return
	}
	if delta := overflowDelta(l.overflow, l.haveOverflow, got); delta != 0 {
		c.kernelDrops.Add(delta)
	}
	l.overflow, l.haveOverflow = got, true
}

// overflowDelta returns how many newly dropped datagrams a reading of the
// kernel's cumulative counter represents. The counter is 32 bits and wraps,
// which unsigned subtraction handles.
func overflowDelta(previous uint32, seen bool, current uint32) uint64 {
	if !seen {
		return uint64(current)
	}
	return uint64(current - previous)
}
