// Package server implements carillon's safe NTP server: request validation,
// default-deny ACLs, bounded rate limiting, AES-CMAC authentication and the
// mode-3 to mode-4 response path. Network listeners are thin adapters around
// Handler so the packet and security rules remain deterministic and testable.
package server

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"carillon/internal/ntp"
	"carillon/internal/ntp/auth"
)

const defaultMinPoll = 6

// SystemStatus is the immutable subset of engine status placed on the wire.
type SystemStatus struct {
	Synced         bool
	Leap           ntp.Leap
	Stratum        uint8
	Precision      int8
	RootDelay      float64
	RootDispersion float64
	ReferenceID    ntp.RefID
	ReferenceTime  time.Time
}

// Config configures one request handler. A listener gets its own Handler so
// its rate-limit table has a single goroutine owner; Stats may be shared.
type Config struct {
	Allow      []netip.Prefix
	Deny       []netip.Prefix
	RequireKey map[netip.Prefix]uint32
	Keys       auth.Keys

	RateLimitPPS float64
	RateBurst    float64
	MaxClients   int
	KoD          bool
	MinPoll      int8

	Status func() SystemStatus
	Now    func() time.Time
	Stats  *Stats
}

type keyRule struct {
	prefix netip.Prefix
	keyID  uint32
}

// Handler processes requests for one UDP listener. It is not safe for
// concurrent use; the listener goroutine is its sole owner.
type Handler struct {
	allow      []netip.Prefix
	deny       []netip.Prefix
	requireKey []keyRule
	keys       auth.Keys
	limiter    *rateLimiter
	kod        bool
	minPoll    int8
	status     func() SystemStatus
	now        func() time.Time
	stats      *Stats

	// publishedClients is this handler's last contribution to the shared
	// client-count gauge, so several listeners of the same address family
	// each add their own delta instead of overwriting one another.
	publishedClients int64
}

// NewHandler validates cfg and returns a handler ready to receive packets.
func NewHandler(cfg Config) (*Handler, error) {
	var errs []error
	if len(cfg.Allow) == 0 {
		errs = append(errs, errors.New("server: allow must contain at least one prefix"))
	}
	if !(cfg.RateLimitPPS > 0) {
		errs = append(errs, fmt.Errorf("server: rate limit %v must be greater than zero", cfg.RateLimitPPS))
	}
	if !(cfg.RateBurst >= 1) {
		errs = append(errs, fmt.Errorf("server: rate burst %v must be at least one", cfg.RateBurst))
	}
	if cfg.MaxClients < 0 {
		errs = append(errs, fmt.Errorf("server: max clients %d must not be negative", cfg.MaxClients))
	}
	if cfg.Status == nil {
		errs = append(errs, errors.New("server: nil status provider"))
	}
	if cfg.Now == nil {
		errs = append(errs, errors.New("server: nil clock"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if cfg.MinPoll == 0 {
		cfg.MinPoll = defaultMinPoll
	}
	if cfg.MaxClients == 0 {
		cfg.MaxClients = DefaultMaxClients
	}
	if cfg.Stats == nil {
		cfg.Stats = &Stats{}
	}

	h := &Handler{
		allow:   maskedPrefixes(cfg.Allow),
		deny:    maskedPrefixes(cfg.Deny),
		keys:    cfg.Keys,
		limiter: newRateLimiter(cfg.RateLimitPPS, cfg.RateBurst, cfg.MaxClients),
		kod:     cfg.KoD,
		minPoll: cfg.MinPoll,
		status:  cfg.Status,
		now:     cfg.Now,
		stats:   cfg.Stats,
	}
	for prefix, keyID := range cfg.RequireKey {
		h.requireKey = append(h.requireKey, keyRule{prefix: prefix.Masked(), keyID: keyID})
	}
	// A more-specific authentication rule wins when configured prefixes
	// overlap. Sorting also removes map iteration order from behavior.
	slices.SortFunc(h.requireKey, func(a, b keyRule) int {
		return b.prefix.Bits() - a.prefix.Bits()
	})
	return h, nil
}

func maskedPrefixes(in []netip.Prefix) []netip.Prefix {
	out := make([]netip.Prefix, len(in))
	for i, p := range in {
		out[i] = p.Masked()
	}
	return out
}

// isClientRequest reports whether a decoded packet asks us for the time.
//
// NTPv1 predates a meaningful mode field and such clients commonly send mode
// 0; ntpd answers those as client requests and so do we. Every other mode is
// refused — in particular modes 6 and 7, the control and private modes that
// every NTP amplification attack has been built on.
func isClientRequest(p *ntp.Packet) bool {
	return p.Mode == ntp.ModeClient || (p.Mode == ntp.ModeReserved && p.Version == 1)
}

// Handle validates request from client and returns its response, or nil when
// the datagram must be dropped. receive is the kernel's CLOCK_REALTIME receive
// timestamp; monotonicNow is used only for rate limiting and expiry.
//
// Every non-nil response is no longer than request. Extension fields are not
// echoed; a verified AES-CMAC request receives a 68-byte authenticated reply.
// Every drop increments exactly one counter, so an operator can chart what was
// refused as well as what was served.
func (h *Handler) Handle(request []byte, client netip.AddrPort, receive, monotonicNow time.Time) []byte {
	addr := client.Addr().Unmap()
	c := h.stats.family(addr)

	pkt, mac, macOffset, err := ntp.Decode(request)
	if err != nil {
		if errors.Is(err, ntp.ErrVersion) {
			c.badVersion.Add(1)
		} else {
			c.malformed.Add(1)
		}
		return nil
	}
	if !isClientRequest(&pkt) {
		c.nonClient.Add(1)
		c.modes[pkt.Mode&7].Add(1)
		return nil
	}
	if martianSource(client) {
		c.martian.Add(1)
		return nil
	}
	if !h.permitted(addr) {
		c.denied.Add(1)
		return nil
	}
	c.versions[pkt.Version].Add(1)
	storeLatest(&c.lastRequest, receive)

	allowed, sendKoD := h.limiter.allow(addr, monotonicNow)
	h.publishClients(c)
	if !allowed {
		c.rateLimited.Add(1)
		if !h.kod || !sendKoD {
			return nil
		}
		c.kod.Add(1)
		return h.reply(&pkt, receive, h.status(), ntp.KissRATE, nil, true)
	}

	var replyKey *auth.Key
	if mac != nil && !mac.IsCryptoNAK() {
		if key, known := h.keys[mac.KeyID]; known {
			if !key.Verify(request[:macOffset], mac) {
				c.badAuth.Add(1)
				return nil
			}
			replyKey = &key
		}
	}
	if required := h.requiredKey(addr); required != 0 {
		if replyKey == nil || replyKey.ID != required {
			c.badAuth.Add(1)
			return nil
		}
	}

	st := h.status()
	refID := st.ReferenceID
	if !st.Synced && refID == (ntp.RefID{}) {
		refID = ntp.KissINIT
	}
	response := h.reply(&pkt, receive, st, refID, replyKey, !st.Synced)
	if response != nil {
		c.served.Add(1)
		storeLatest(&c.lastServed, receive)
		if !st.Synced {
			c.unsynced.Add(1)
		}
	}
	return response
}

// publishClients folds this handler's rate-limit table size into the shared
// gauge. It runs on the listener goroutine, so publishedClients needs no
// synchronization of its own.
func (h *Handler) publishClients(c *counters) {
	n := int64(h.limiter.size())
	if delta := n - h.publishedClients; delta != 0 {
		c.clients.Add(delta)
		h.publishedClients = n
	}
}

func (h *Handler) permitted(addr netip.Addr) bool {
	if matches(h.deny, addr) {
		return false
	}
	return matches(h.allow, addr)
}

func matches(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func (h *Handler) requiredKey(addr netip.Addr) uint32 {
	for _, r := range h.requireKey {
		if r.prefix.Contains(addr) {
			return r.keyID
		}
	}
	return 0
}

func (h *Handler) reply(req *ntp.Packet, receive time.Time, st SystemStatus, refID ntp.RefID, key *auth.Key, special bool) []byte {
	p := ntp.Packet{
		Leap:           st.Leap,
		Version:        req.Version,
		Mode:           ntp.ModeServer,
		Stratum:        st.Stratum,
		Poll:           req.Poll,
		Precision:      st.Precision,
		RootDelay:      ntp.ShortFromSeconds(st.RootDelay),
		RootDispersion: ntp.ShortFromSeconds(st.RootDispersion),
		ReferenceID:    refID,
		ReferenceTime:  ntp.FromTime(st.ReferenceTime),
		OriginTime:     req.TransmitTime,
		ReceiveTime:    ntp.FromTime(receive),
	}
	if special {
		p.Leap = ntp.LeapUnsync
		p.RootDelay = 0
		p.ReferenceTime = 0
		if refID == ntp.KissRATE {
			p.Stratum = 0
			p.Poll = h.minPoll
			p.RootDispersion = 0
		} else {
			p.Stratum = 16
			p.RootDispersion = ntp.ShortFromSeconds(16)
		}
	}
	// Keep this read as close to the eventual send as packet construction
	// permits. Network code calls Handle immediately before WriteMsgUDP.
	p.TransmitTime = ntp.FromTime(h.now())
	out := p.Marshal()
	if key != nil {
		out = key.Append(out)
	}
	return out
}
