package source

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"carillon/internal/clock"
	"carillon/internal/discipline"
	"carillon/internal/ntp"
	"carillon/internal/ntp/auth"
	"carillon/internal/sockts"
)

const (
	// defaultTimeout is how long a poll waits for a reply.
	defaultTimeout = 2 * time.Second

	// resolveAfterTimeouts re-resolves a hostname after this many consecutive
	// unanswered polls: the server may have moved. There is deliberately no
	// periodic re-resolution: a pool name answers with a different server on
	// every lookup, and changing servers under a running clock filter blends
	// unrelated measurements (DESIGN.md §5.4).
	resolveAfterTimeouts = 8

	// burstCount and burstSpacing describe an iburst: packets sent while the
	// server is unreachable so the first fix arrives quickly.
	burstCount   = 4
	burstSpacing = 2 * time.Second

	// unreachableBackoff is the number of consecutive misses after which
	// the poll interval grows by one step per further miss.
	unreachableBackoff = 10

	// maxDelay is the round-trip beyond which a reply is discarded.
	maxDelay = 16.0

	// maxRootDistance is the server root distance beyond which a reply is
	// discarded (the server itself is not usefully synchronized).
	maxRootDistance = 1.5

	// maxFutureRefTime is how far a server's reference time may lie after
	// its transmit time before the reply is discarded.
	maxFutureRefTime = 1.0

	// pollJitter is the fractional randomization of the poll interval.
	pollJitter = 0.05

	// logEvery throttles repeated warnings: the first occurrence is logged,
	// then every logEvery-th.
	logEvery = 10
)

// NTPConfig configures an NTP client source.
type NTPConfig struct {
	// Name identifies the source in logs and status; defaults to Address.
	Name string

	// Address is the server: hostname or IP literal, optional port.
	Address string

	// Key, when set, authenticates requests and requires authenticated
	// replies (RFC 8573 AES-128-CMAC).
	Key *auth.Key

	// IBurst sends a burst of four packets while the server is unreachable.
	IBurst bool

	// PollMin and PollMax bound the poll exponent (log2 seconds). Zero
	// selects the defaults 6 and 10.
	PollMin int8
	PollMax int8

	// Timeout is the reply timeout per request; zero selects 2 s.
	Timeout time.Duration

	// NoKernelTimestamps disables kernel receive timestamps. Set it when clk
	// is not the system clock (a simulated clock in tests): a kernel
	// timestamp is only meaningful on the clock it was taken from.
	NoKernelTimestamps bool

	// Generation returns the engine's measurement epoch. The source reads
	// it before T1 and again before the reply is filtered; a change means
	// the clock was stepped mid-exchange and the sample is unusable. Nil
	// disables the check and stamps every measurement 0.
	Generation func() uint64
}

// NTP is an NTP client source polling one server.
type NTP struct {
	cfg  NTPConfig
	host string
	port uint16
	clk  clock.Clock
	log  *slog.Logger

	filter         *discipline.Filter
	resetRequested atomic.Bool
	info           atomic.Pointer[Info]

	// sleep waits for d or until ctx is done, returning false in the latter
	// case. Tests replace it to run without real delays.
	sleep func(ctx context.Context, d time.Duration) bool

	// lookup turns host:port into an address. Tests replace it to steer the
	// poller between servers.
	lookup func(ctx context.Context, host string, port uint16) (netip.AddrPort, error)

	// Poller state, touched only by the Run goroutine.
	addr                netip.AddrPort
	haveAddr            bool
	resolveFailing      bool
	reach               uint8
	poll                int8
	consecutiveTimeouts int
	consecutiveMisses   int
	consecutiveErrs     uint64
	denied              bool
	tsWarned            bool
	badAuthSeen         uint64
	bogusSeen           uint64
	staleSeen           uint64
}

// NewNTP validates cfg and returns a source ready to Run.
func NewNTP(cfg NTPConfig, clk clock.Clock, log *slog.Logger) (*NTP, error) {
	if cfg.Name == "" {
		cfg.Name = cfg.Address
	}
	host, port, err := splitHostPort(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", cfg.Name, err)
	}
	if cfg.PollMin == 0 {
		cfg.PollMin = 6
	}
	if cfg.PollMax == 0 {
		cfg.PollMax = 10
	}
	if cfg.PollMin < discipline.MinPoll || cfg.PollMax > discipline.MaxPoll || cfg.PollMin > cfg.PollMax {
		return nil, fmt.Errorf("source %q: poll range %d..%d outside %d..%d", cfg.Name, cfg.PollMin, cfg.PollMax, discipline.MinPoll, discipline.MaxPoll)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Key != nil && len(cfg.Key.Secret) != 16 {
		return nil, fmt.Errorf("source %q: key %d is not 16 bytes", cfg.Name, cfg.Key.ID)
	}
	if clk == nil {
		return nil, fmt.Errorf("source %q: nil clock", cfg.Name)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n := &NTP{
		cfg:    cfg,
		host:   host,
		port:   port,
		clk:    clk,
		log:    log.With("source", cfg.Name),
		filter: discipline.NewFilter(ntp.Log2Seconds(clk.Precision())),
		sleep:  realSleep,
		lookup: resolve,
		poll:   cfg.PollMin,
	}
	n.info.Store(&Info{Name: cfg.Name, Address: cfg.Address, Poll: cfg.PollMin})
	return n, nil
}

// generation reads the engine's measurement epoch, or 0 when the source was
// built without one.
func (n *NTP) generation() uint64 {
	if n.cfg.Generation == nil {
		return 0
	}
	return n.cfg.Generation()
}

// Name implements Source.
func (n *NTP) Name() string { return n.cfg.Name }

// Info implements Source.
func (n *NTP) Info() Info { return *n.info.Load() }

// Reset implements Source.
func (n *NTP) Reset() { n.resetRequested.Store(true) }

// Run implements Source.
func (n *NTP) Run(ctx context.Context, out chan<- discipline.Measurement) error {
	for {
		if n.denied {
			<-ctx.Done()
			return nil
		}
		switch {
		case !n.ensureResolved(ctx):
			if ctx.Err() != nil {
				return nil
			}
			n.miss("resolve failed")
			if !n.emit(ctx, out, nil, n.generation()) {
				return nil
			}
		case n.cfg.IBurst && n.reach == 0:
			for i := 0; i < burstCount && !n.denied; i++ {
				if i > 0 && !n.sleep(ctx, burstSpacing) {
					return nil
				}
				if !n.pollOnce(ctx, out) {
					return nil
				}
			}
		default:
			if !n.pollOnce(ctx, out) {
				return nil
			}
		}
		if n.denied {
			continue
		}
		if !n.sleep(ctx, pollInterval(n.poll)) {
			return nil
		}
	}
}

// ensureResolved makes sure n.addr is usable, resolving the hostname when
// it has never been resolved or when the server has stopped answering. A
// name that resolves to a different server than before discards the clock
// filter, because its samples describe the previous server. It returns
// false when no address is available for this poll.
func (n *NTP) ensureResolved(ctx context.Context) bool {
	if n.haveAddr && n.consecutiveTimeouts < resolveAfterTimeouts {
		return true
	}
	addr, err := n.lookup(ctx, n.host, n.port)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if !n.resolveFailing {
			n.log.Warn("cannot resolve server", "address", n.cfg.Address, "err", err)
			n.resolveFailing = true
		}
		n.updateInfo(func(i *Info) { i.LastError = err.Error() })
		// A stale address is better than none while DNS is unavailable.
		return n.haveAddr
	}
	if n.resolveFailing {
		n.log.Info("server resolves again", "address", n.cfg.Address, "resolved", addr)
		n.resolveFailing = false
	}
	switch {
	case !n.haveAddr:
		n.log.Info("resolved server", "address", n.cfg.Address, "resolved", addr)
	case addr != n.addr:
		n.filter.Reset()
		n.log.Info("resolved server", "address", n.cfg.Address, "resolved", addr, "previous", n.addr)
	}
	n.addr = addr
	n.haveAddr = true
	n.consecutiveTimeouts = 0
	n.updateInfo(func(i *Info) { i.Resolved = addr })
	return true
}

// resolve turns host:port into an address, taking the first answer.
func resolve(ctx context.Context, host string, port uint16) (netip.AddrPort, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(a.Unmap(), port), nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("lookup %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("lookup %q: no addresses", host)
	}
	return netip.AddrPortFrom(addrs[0].Unmap(), port), nil
}

// sample is a usable reply after filtering.
type sample struct {
	out exchangeResult
	f   discipline.Output
}

// pollOnce performs one exchange, updates state, and emits a measurement.
// It returns false when ctx is done.
func (n *NTP) pollOnce(ctx context.Context, out chan<- discipline.Measurement) bool {
	gen := n.generation()
	res, err := exchange(ctx, exchangeParams{
		addr:     n.addr,
		key:      n.cfg.Key,
		poll:     n.poll,
		timeout:  n.cfg.Timeout,
		clk:      n.clk,
		kernelTS: !n.cfg.NoKernelTimestamps,
		log:      n.log,
		tsWarned: &n.tsWarned,
	})
	if res.Sent {
		n.updateInfo(func(i *Info) { i.Sent++ })
	}
	var s *sample
	var kiss *kissError
	var bogus *bogusError
	switch {
	case err == nil:
		if now := n.generation(); now != gen {
			// The clock was stepped between T1 and T4: the offset and the
			// delay are both wrong by the step. Reachability is real, so
			// record the reply, but keep the sample out of the filter.
			n.discardStale(now)
		} else {
			s = n.hit(res)
		}
	case ctx.Err() != nil:
		return false
	case errors.Is(err, errTimeout):
		n.consecutiveTimeouts++
		n.updateInfo(func(i *Info) { i.Timeouts++; i.LastError = "timeout" })
		n.miss("timeout")
	case errors.Is(err, errBadAuth):
		n.badAuthSeen++
		if n.badAuthSeen == 1 || n.badAuthSeen%logEvery == 0 {
			n.log.Warn("reply failed authentication", "count", n.badAuthSeen)
		}
		n.updateInfo(func(i *Info) { i.BadAuth++; i.LastError = "authentication failed" })
		n.miss("bad auth")
	case errors.As(err, &kiss):
		n.handleKiss(kiss)
	case errors.As(err, &bogus):
		n.bogusSeen++
		if n.bogusSeen == 1 || n.bogusSeen%logEvery == 0 {
			n.log.Warn("discarding reply", "reason", bogus.reason, "count", n.bogusSeen)
		}
		n.updateInfo(func(i *Info) { i.Bogus++; i.LastError = "bogus: " + bogus.reason })
		n.miss("bogus")
	default:
		if n.shouldLogErr() {
			n.log.Warn("exchange failed", "err", err, "count", n.consecutiveErrs)
		}
		n.updateInfo(func(i *Info) { i.LastError = err.Error() })
		n.miss("error")
	}
	if s == nil {
		// Nothing was measured, so the measurement is valid under whatever
		// generation is current now.
		gen = n.generation()
	}
	return n.emit(ctx, out, s, gen)
}

// discardStale records a reply whose exchange spanned a clock step or leap
// reset. It counts as reached — the server answered — but carries no usable
// offset.
func (n *NTP) discardStale(now uint64) {
	n.reach = n.reach<<1 | 1
	n.consecutiveTimeouts = 0
	n.consecutiveMisses = 0
	n.consecutiveErrs = 0
	n.staleSeen++
	if n.staleSeen == 1 || n.staleSeen%logEvery == 0 {
		n.log.Info("discarding a reply that spans a clock step", "count", n.staleSeen, "generation", now)
	}
	// Received counts usable replies; this one is not one.
	n.updateInfo(func(i *Info) { i.Stale++; i.Reach = n.reach; i.Poll = n.poll })
}

// handleKiss applies a kiss-o'-death reply.
func (n *NTP) handleKiss(k *kissError) {
	n.updateInfo(func(i *Info) { i.Kiss++; i.LastError = "kiss " + k.code })
	switch k.code {
	case "RATE":
		want := n.poll + 1
		if k.pkt.Poll > want {
			want = k.pkt.Poll
		}
		want = clampPoll(want, n.cfg.PollMin, n.cfg.PollMax)
		n.log.Warn("server asked us to slow down", "old_poll", n.poll, "new_poll", want)
		n.poll = want
		n.miss("kiss RATE")
	case "DENY", "RSTR":
		if k.authenticated {
			n.log.Error("server denied access; polling stopped", "code", k.code)
			n.denied = true
			n.miss("kiss " + k.code)
			n.reach = 0
			n.updateInfo(func(i *Info) { i.Denied = true; i.Reach = 0 })
			return
		}
		n.log.Warn("unauthenticated access-denied kiss ignored", "code", k.code)
		n.miss("kiss " + k.code)
	default:
		n.log.Warn("unknown kiss code", "code", k.code)
		n.miss("kiss " + k.code)
	}
}

// hit records a usable reply and runs it through the clock filter. It
// returns nil when the filter did not produce a new estimate.
func (n *NTP) hit(res exchangeResult) *sample {
	wasUnreachable := n.reach == 0
	n.reach = n.reach<<1 | 1
	n.consecutiveTimeouts = 0
	n.consecutiveMisses = 0
	n.consecutiveErrs = 0
	if wasUnreachable {
		n.poll = n.cfg.PollMin
		n.log.Info("server reachable", "resolved", n.addr)
	}
	if n.resetRequested.Swap(false) {
		n.filter.Reset()
	}
	pkt := res.Packet
	disp := ntp.Log2Seconds(n.clk.Precision()) +
		ntp.Log2Seconds(clampPrecision(pkt.Precision)) +
		discipline.Phi*res.T4.Sub(res.T1).Seconds()
	f, updated := n.filter.Add(res.Offset, res.Delay, disp, n.clk.Monotonic())
	n.updateInfo(func(i *Info) {
		i.Received++
		i.LastRx = res.T4
		i.LastError = ""
		i.Stratum = pkt.Stratum
		i.RefID = pkt.ReferenceID
		i.Leap = pkt.Leap
		if !res.KernelTS {
			i.NoKernelTS++
		}
		if updated {
			i.Offset, i.Delay, i.Dispersion, i.Jitter = f.Offset, f.Delay, f.Dispersion, f.Jitter
		}
	})
	n.log.Debug("reply", "offset", res.Offset, "delay", res.Delay, "stratum", pkt.Stratum, "kernel_ts", res.KernelTS, "updated", updated)
	if !updated {
		n.updateInfo(func(i *Info) { i.Reach = n.reach; i.Poll = n.poll })
		return nil
	}
	n.poll = adaptPoll(n.poll, f.Offset, f.Jitter, n.cfg.PollMin, n.cfg.PollMax)
	n.updateInfo(func(i *Info) { i.Reach = n.reach; i.Poll = n.poll })
	return &sample{out: res, f: f}
}

// shouldLogErr counts one failed exchange and reports whether it deserves a
// log line: the first, then every logEvery-th, the same throttle badAuthSeen
// and bogusSeen use. A server that stays down fails every poll, and an iburst
// makes that four failures a cycle, so logging each one buries every other
// line in the file. miss() logs the transition to unreachable separately, and
// hit() clears the count on recovery.
//
// Failures are counted, never compared: every exchange opens a fresh socket,
// so the error text carries a different ephemeral source port each time and
// no two failures would ever compare equal. Suppressing on "same text as last
// time" therefore suppresses nothing at all.
func (n *NTP) shouldLogErr() bool {
	n.consecutiveErrs++
	return n.consecutiveErrs == 1 || n.consecutiveErrs%logEvery == 0
}

// miss records a poll without a usable reply.
func (n *NTP) miss(reason string) {
	wasReachable := n.reach != 0
	n.reach <<= 1
	n.consecutiveMisses++
	if n.reach == 0 {
		if wasReachable {
			n.log.Warn("server unreachable", "reason", reason)
		}
		if n.consecutiveMisses >= unreachableBackoff && n.poll < n.cfg.PollMax {
			n.poll++
		}
	}
	n.updateInfo(func(i *Info) { i.Reach = n.reach; i.Poll = n.poll })
}

// emit sends the measurement for the poll that just completed.
func (n *NTP) emit(ctx context.Context, out chan<- discipline.Measurement, s *sample, gen uint64) bool {
	m := discipline.Measurement{
		Source:     n.cfg.Name,
		Now:        n.clk.Monotonic(),
		Reach:      n.reach,
		Poll:       n.poll,
		Generation: gen,
	}
	if s != nil {
		pkt := s.out.Packet
		m.Valid = true
		m.At = s.f.At
		m.Offset = s.f.Offset
		m.Delay = s.f.Delay
		m.Dispersion = s.f.Dispersion
		m.Jitter = s.f.Jitter
		m.Leap = pkt.Leap
		m.Stratum = pkt.Stratum
		m.RefID = pkt.ReferenceID
		m.SourceRefID = ntp.RefIDFromAddr(n.addr.Addr())
		m.RootDelay = pkt.RootDelay.Seconds()
		m.RootDisp = pkt.RootDispersion.Seconds()
		m.Precision = pkt.Precision
		m.RefTime = pkt.ReferenceTime.Time(s.out.T1)
	}
	select {
	case out <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

// updateInfo publishes a modified copy of the status snapshot.
func (n *NTP) updateInfo(f func(i *Info)) {
	cur := *n.info.Load()
	f(&cur)
	n.info.Store(&cur)
}

func realSleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Errors and error types produced by exchange.
var (
	errTimeout = errors.New("no reply before timeout")
	errBadAuth = errors.New("reply failed authentication")
)

// bogusError is a reply that was addressed to us but is unusable.
type bogusError struct{ reason string }

func (e *bogusError) Error() string { return "bogus reply: " + e.reason }

// kissError is a kiss-o'-death reply.
type kissError struct {
	code          string
	pkt           ntp.Packet
	authenticated bool
}

func (e *kissError) Error() string { return "kiss-o'-death " + e.code }

type exchangeParams struct {
	addr     netip.AddrPort
	key      *auth.Key
	poll     int8
	timeout  time.Duration
	clk      clock.Clock
	kernelTS bool
	log      *slog.Logger
	tsWarned *bool
}

// exchangeResult is the outcome of one request/reply.
type exchangeResult struct {
	Sent          bool
	Packet        ntp.Packet
	MAC           *ntp.MAC
	Authenticated bool
	T1, T4        time.Time
	KernelTS      bool
	Offset        float64
	Delay         float64
}

// exchange performs one client/server exchange on a fresh socket.
func exchange(ctx context.Context, p exchangeParams) (exchangeResult, error) {
	var res exchangeResult

	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return res, fmt.Errorf("nonce: %w", err)
	}
	req := ntp.Packet{
		Version:      ntp.Version,
		Mode:         ntp.ModeClient,
		Poll:         p.poll,
		Precision:    p.clk.Precision(),
		TransmitTime: ntp.Time(binary.BigEndian.Uint64(nonce[:])),
	}
	buf := req.Marshal()
	if p.key != nil {
		buf = p.key.Append(buf)
	}

	addr := p.addr
	network := "udp6"
	if a := addr.Addr().Unmap(); a.Is4() {
		network = "udp4"
		addr = netip.AddrPortFrom(a, addr.Port())
	}
	conn, err := net.ListenUDP(network, nil)
	if err != nil {
		return res, fmt.Errorf("socket: %w", err)
	}
	defer conn.Close()

	if p.kernelTS {
		rc, rerr := conn.SyscallConn()
		if rerr == nil {
			rerr = sockts.Enable(rc)
		}
		if rerr != nil && p.tsWarned != nil && !*p.tsWarned {
			p.log.Warn("kernel receive timestamps unavailable; using user-space timestamps", "err", rerr)
			*p.tsWarned = true
		}
	}

	// Cancellation mid-read: expire the deadline so the read returns.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()
	if err := conn.SetReadDeadline(time.Now().Add(p.timeout)); err != nil {
		return res, fmt.Errorf("deadline: %w", err)
	}

	t1 := p.clk.Now()
	if _, err := conn.WriteToUDPAddrPort(buf, addr); err != nil {
		return res, fmt.Errorf("send to %s: %w", addr, err)
	}
	res.Sent = true
	res.T1 = t1

	rbuf := make([]byte, ntp.MaxPacketSize)
	oob := make([]byte, sockts.OOBSize)
	for {
		nr, oobn, _, from, err := conn.ReadMsgUDPAddrPort(rbuf, oob)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return res, errTimeout
			}
			return res, fmt.Errorf("receive: %w", err)
		}
		t4 := p.clk.Now()
		kernel := false
		if p.kernelTS {
			if kt, ok := sockts.Parse(oob[:oobn]); ok {
				t4, kernel = kt, true
			}
		}
		if !sameEndpoint(from, addr) {
			continue
		}
		pkt, mac, macOff, err := ntp.Decode(rbuf[:nr])
		if err != nil {
			continue
		}
		if pkt.OriginTime != req.TransmitTime {
			continue
		}

		res.Packet, res.MAC, res.T4, res.KernelTS = pkt, mac, t4, kernel
		if pkt.Mode != ntp.ModeServer {
			return res, &bogusError{"mode " + pkt.Mode.String()}
		}
		if p.key != nil {
			if mac == nil || mac.IsCryptoNAK() || !p.key.Verify(rbuf[:macOff], mac) {
				return res, errBadAuth
			}
			res.Authenticated = true
		}
		if pkt.IsKiss() {
			code := pkt.KissCode()
			if code == "" {
				return res, &bogusError{"stratum 0 without a kiss code"}
			}
			return res, &kissError{code: code, pkt: pkt, authenticated: res.Authenticated}
		}
		if pkt.Stratum > 15 {
			return res, &bogusError{fmt.Sprintf("stratum %d", pkt.Stratum)}
		}
		if pkt.Leap == ntp.LeapUnsync {
			return res, &bogusError{"server unsynchronized"}
		}
		if pkt.TransmitTime.IsZero() || pkt.ReceiveTime.IsZero() {
			return res, &bogusError{"zero timestamp"}
		}
		t1n, t4n := ntp.FromTime(t1), ntp.FromTime(t4)
		delay := t4n.Sub(t1n) - pkt.TransmitTime.Sub(pkt.ReceiveTime)
		// A round trip cannot be negative, but two clocks' granularity can
		// make it read that way by up to the sum of the precisions.
		tolerance := ntp.Log2Seconds(p.clk.Precision()) + ntp.Log2Seconds(clampPrecision(pkt.Precision))
		if delay < -tolerance || delay >= maxDelay {
			return res, &bogusError{fmt.Sprintf("delay %.6f s", delay)}
		}
		if delay < 0 {
			delay = 0
		}
		if rd := pkt.RootDelay.Seconds()/2 + pkt.RootDispersion.Seconds(); rd >= maxRootDistance {
			return res, &bogusError{fmt.Sprintf("root distance %.3f s", rd)}
		}
		if pkt.ReferenceTime.Sub(pkt.TransmitTime) > maxFutureRefTime {
			return res, &bogusError{"reference time in the future"}
		}
		res.Offset = (pkt.ReceiveTime.Sub(t1n) + pkt.TransmitTime.Sub(t4n)) / 2
		res.Delay = delay
		return res, nil
	}
}

// Result is the outcome of a one-shot Query.
type Result struct {
	Server         netip.AddrPort
	Offset         float64
	Delay          float64
	Packet         ntp.Packet
	T1, T2, T3, T4 time.Time
	KernelTS       bool
	Authenticated  bool
}

// Query performs a single exchange with a server, for diagnostics. A kiss
// code, authentication failure, or unusable reply is returned as an error
// that says why.
func Query(ctx context.Context, address string, key *auth.Key, clk clock.Clock, timeout time.Duration) (Result, error) {
	host, port, err := splitHostPort(address)
	if err != nil {
		return Result{}, err
	}
	addr, err := resolve(ctx, host, port)
	if err != nil {
		return Result{}, err
	}
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	warned := false
	res, err := exchange(ctx, exchangeParams{
		addr: addr, key: key, poll: 6, timeout: timeout, clk: clk, kernelTS: true,
		log: slog.New(slog.DiscardHandler), tsWarned: &warned,
	})
	if err != nil {
		return Result{Server: addr}, fmt.Errorf("query %s: %w", addr, err)
	}
	return Result{
		Server:        addr,
		Offset:        res.Offset,
		Delay:         res.Delay,
		Packet:        res.Packet,
		T1:            res.T1,
		T2:            res.Packet.ReceiveTime.Time(res.T1),
		T3:            res.Packet.TransmitTime.Time(res.T1),
		T4:            res.T4,
		KernelTS:      res.KernelTS,
		Authenticated: res.Authenticated,
	}, nil
}

var _ Source = (*NTP)(nil)
