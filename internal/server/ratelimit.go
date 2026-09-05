package server

import (
	"container/list"
	"net/netip"
	"time"
)

// DefaultMaxClients bounds the rate-limit table when [serve] max_clients is
// unset. Eviction is least-recently-used, so under a flood of forged source
// addresses the heavy hitters stay tracked and the one-shot tail is what gets
// dropped from the table. It is exported so the configuration default and the
// handler fallback cannot drift apart.
const DefaultMaxClients = 65536

const (
	clientIdleExpiry = 60 * time.Second
	kodInterval      = 4 * time.Second

	// DefaultRateLimitV6Prefix is the IPv6 prefix length a bucket covers
	// when [serve] rate_limit_v6_prefix is unset. Every residential IPv6
	// customer controls at least a /64, so keying on the full /128 gives
	// one host an unbounded supply of fresh buckets and fresh LRU slots
	// with which to evict real clients.
	DefaultRateLimitV6Prefix = 64
)

// bucketKey identifies a token bucket. Authenticated requests get their own
// keyspace: the bucket for an address is otherwise shared with anyone able to
// forge that address, who could then drain the authenticated peer's tokens
// from anywhere on the path and break exactly the association the shared key
// exists to protect.
type bucketKey struct {
	addr  netip.Addr // masked to the family's bucket prefix
	keyID uint32     // 0 for unauthenticated traffic
}

// rateLimiter is a bounded per-address token-bucket table. It is owned by
// one listener goroutine, so it needs no locks.
type rateLimiter struct {
	rate       float64
	burst      float64
	maxClients int
	v6Prefix   int
	clients    map[bucketKey]*clientBucket
	lru        list.List // most recently used at the front
}

type clientBucket struct {
	key      bucketKey
	tokens   float64
	updated  time.Time
	lastSeen time.Time
	lastKoD  time.Time
	elem     *list.Element
}

func newRateLimiter(rate, burst float64, maxClients, v6Prefix int) *rateLimiter {
	if maxClients <= 0 {
		maxClients = DefaultMaxClients
	}
	if v6Prefix <= 0 || v6Prefix > 128 {
		v6Prefix = DefaultRateLimitV6Prefix
	}
	return &rateLimiter{
		rate:       rate,
		burst:      burst,
		maxClients: maxClients,
		v6Prefix:   v6Prefix,
		clients:    make(map[bucketKey]*clientBucket),
	}
}

// key masks a client address down to the unit a bucket covers: the whole
// address for IPv4, the configured prefix (a /64 by default) for IPv6.
func (l *rateLimiter) key(addr netip.Addr, keyID uint32) bucketKey {
	addr = addr.Unmap()
	if addr.Is6() && l.v6Prefix < 128 {
		if p, err := addr.Prefix(l.v6Prefix); err == nil {
			addr = p.Addr()
		}
	}
	return bucketKey{addr: addr, keyID: keyID}
}

// size reports how many clients the table is tracking. Entries expire after
// clientIdleExpiry, so this is roughly the number of distinct clients seen in
// the last minute.
func (l *rateLimiter) size() int { return len(l.clients) }

// allow reports whether a packet may be served and, when it may not, whether
// enough time has elapsed to send this client another RATE kiss packet.
func (l *rateLimiter) allow(k bucketKey, now time.Time) (allowed, sendKoD bool) {
	l.expire(now)
	b := l.clients[k]
	if b == nil {
		if len(l.clients) >= l.maxClients {
			l.evictOldest()
		}
		b = &clientBucket{key: k, tokens: l.burst, updated: now, lastSeen: now}
		b.elem = l.lru.PushFront(b)
		l.clients[k] = b
	} else {
		elapsed := now.Sub(b.updated).Seconds()
		if elapsed > 0 {
			b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		}
		b.updated = now
		b.lastSeen = now
		l.lru.MoveToFront(b.elem)
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, false
	}
	if b.lastKoD.IsZero() || now.Sub(b.lastKoD) >= kodInterval {
		b.lastKoD = now
		return false, true
	}
	return false, false
}

func (l *rateLimiter) expire(now time.Time) {
	for {
		e := l.lru.Back()
		if e == nil {
			return
		}
		b := e.Value.(*clientBucket)
		if now.Sub(b.lastSeen) <= clientIdleExpiry {
			return
		}
		l.remove(b)
	}
}

func (l *rateLimiter) evictOldest() {
	if e := l.lru.Back(); e != nil {
		l.remove(e.Value.(*clientBucket))
	}
}

func (l *rateLimiter) remove(b *clientBucket) {
	delete(l.clients, b.key)
	l.lru.Remove(b.elem)
}
