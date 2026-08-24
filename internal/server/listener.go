package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"carillon/internal/ntp"
	"carillon/internal/sockts"
)

const serverOOBSize = 256

// ServiceConfig configures a set of NTP UDP listeners.
type ServiceConfig struct {
	Listen  []netip.AddrPort
	Handler Config
	Log     *slog.Logger
}

// Service owns all configured UDP sockets. They are opened by Listen before
// the engine starts so bind and socket-option failures are startup failures.
type Service struct {
	listeners []*udpListener
	closeOnce sync.Once
}

type udpListener struct {
	addr    netip.AddrPort
	network string
	conn    *net.UDPConn
	handler *Handler
	log     *slog.Logger
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
		l, err := listenOne(addr, cfg.Handler, cfg.Log)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.listeners = append(s.listeners, l)
	}
	return s, nil
}

func listenOne(addr netip.AddrPort, hcfg Config, log *slog.Logger) (*udpListener, error) {
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
	h, err := NewHandler(hcfg)
	if err != nil {
		return fail("server: listen %s: %w", addr, err)
	}
	actual := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	return &udpListener{addr: actual, network: network, conn: conn, handler: h, log: log.With("listen", actual)}, nil
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
		if flags&syscall.MSG_TRUNC != 0 || n > ntp.MaxPacketSize {
			continue
		}
		received := l.handler.now()
		if ts, ok := sockts.Parse(oob[:oobn]); ok {
			received = ts
		} else if !missingTimestampWarned {
			l.handler.stats.missingKernelTS.Add(1)
			l.log.Warn("kernel receive timestamp missing; using a user-space timestamp")
			missingTimestampWarned = true
		} else {
			l.handler.stats.missingKernelTS.Add(1)
		}
		replyOOB := sourceControl(oob[:oobn], l.network)
		response := l.handler.Handle(buf[:n], from.Addr(), received, time.Now())
		if response == nil {
			continue
		}
		if _, _, err := l.conn.WriteMsgUDPAddrPort(response, replyOOB, from); err != nil {
			l.log.Debug("send failed", "client", from, "error", err)
		}
	}
}
