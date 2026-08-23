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
	"sync/atomic"
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
	if cfg.Stats == nil {
		cfg.Stats = &Stats{}
	}

	h := &Handler{
		allow:   maskedPrefixes(cfg.Allow),
		deny:    maskedPrefixes(cfg.Deny),
		keys:    cfg.Keys,
		limiter: newRateLimiter(cfg.RateLimitPPS, cfg.RateBurst),
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

// Handle validates request from client and returns its response, or nil when
// the datagram must be dropped. receive is the kernel's CLOCK_REALTIME receive
// timestamp; monotonicNow is used only for rate limiting and expiry.
//
// Every non-nil response is no longer than request. Extension fields are not
// echoed; a verified AES-CMAC request receives a 68-byte authenticated reply.
func (h *Handler) Handle(request []byte, client netip.Addr, receive, monotonicNow time.Time) []byte {
	pkt, mac, macOffset, err := ntp.Decode(request)
	if err != nil || pkt.Mode != ntp.ModeClient {
		return nil
	}
	client = client.Unmap()
	if !h.permitted(client) {
		h.stats.denied.Add(1)
		return nil
	}
	allowed, sendKoD := h.limiter.allow(client, monotonicNow)
	if !allowed {
		h.stats.rateLimited.Add(1)
		if !h.kod || !sendKoD {
			return nil
		}
		return h.reply(&pkt, receive, h.status(), ntp.KissRATE, nil, true)
	}

	var replyKey *auth.Key
	if mac != nil && !mac.IsCryptoNAK() {
		if key, known := h.keys[mac.KeyID]; known {
			if !key.Verify(request[:macOffset], mac) {
				h.stats.badAuth.Add(1)
				return nil
			}
			replyKey = &key
		}
	}
	if required := h.requiredKey(client); required != 0 {
		if replyKey == nil || replyKey.ID != required {
			h.stats.badAuth.Add(1)
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
		h.stats.served.Add(1)
		if !st.Synced {
			h.stats.unsynced.Add(1)
		}
	}
	return response
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

// Stats contains lock-free counters shared by all listeners.
type Stats struct {
	served      atomic.Uint64
	denied      atomic.Uint64
	rateLimited atomic.Uint64
	badAuth     atomic.Uint64
	unsynced    atomic.Uint64
}

// StatsSnapshot is a consistent-enough operational view of the independent
// monotonic counters. Exact cross-field simultaneity is not required.
type StatsSnapshot struct {
	Served      uint64 `json:"served"`
	Denied      uint64 `json:"denied"`
	RateLimited uint64 `json:"ratelimited"`
	BadAuth     uint64 `json:"badauth"`
	Unsynced    uint64 `json:"unsynced"`
}

// Snapshot returns the current counters.
func (s *Stats) Snapshot() StatsSnapshot {
	if s == nil {
		return StatsSnapshot{}
	}
	return StatsSnapshot{
		Served:      s.served.Load(),
		Denied:      s.denied.Load(),
		RateLimited: s.rateLimited.Load(),
		BadAuth:     s.badAuth.Load(),
		Unsynced:    s.unsynced.Load(),
	}
}
