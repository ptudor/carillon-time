package server

import (
	"container/list"
	"net/netip"
	"time"
)

const (
	defaultMaxClients = 65536
	clientIdleExpiry  = 60 * time.Second
	kodInterval       = 4 * time.Second
)

// rateLimiter is a bounded per-address token-bucket table. It is owned by
// one listener goroutine, so it needs no locks.
type rateLimiter struct {
	rate       float64
	burst      float64
	maxClients int
	clients    map[netip.Addr]*clientBucket
	lru        list.List // most recently used at the front
}

type clientBucket struct {
	addr     netip.Addr
	tokens   float64
	updated  time.Time
	lastSeen time.Time
	lastKoD  time.Time
	elem     *list.Element
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	return &rateLimiter{
		rate:       rate,
		burst:      burst,
		maxClients: defaultMaxClients,
		clients:    make(map[netip.Addr]*clientBucket),
	}
}

// allow reports whether a packet may be served and, when it may not, whether
// enough time has elapsed to send this client another RATE kiss packet.
func (l *rateLimiter) allow(addr netip.Addr, now time.Time) (allowed, sendKoD bool) {
	addr = addr.Unmap()
	l.expire(now)
	b := l.clients[addr]
	if b == nil {
		if len(l.clients) >= l.maxClients {
			l.evictOldest()
		}
		b = &clientBucket{addr: addr, tokens: l.burst, updated: now, lastSeen: now}
		b.elem = l.lru.PushFront(b)
		l.clients[addr] = b
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
	delete(l.clients, b.addr)
	l.lru.Remove(b.elem)
}
