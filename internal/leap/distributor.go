package leap

import (
	"sync/atomic"
	"time"
)

// budget is a bounded CAS token bucket (GCRA). Its virtual finish time is
// relative to a monotonic epoch; wall-clock steps cannot refill it.
type budget struct{ end atomic.Int64 }

func (b *budget) allow(now int64, interval time.Duration, burst int64) bool {
	for tries := 0; tries < 8; tries++ {
		old := b.end.Load()
		next := max(old, now) + int64(interval)
		if next-now > burst*int64(interval) {
			return false
		}
		if b.end.CompareAndSwap(old, next) {
			return true
		}
	}
	return false // contention drops bounded work, never blocks a listener
}

// Distributor is shared by all listeners. The key map is immutable; only
// fixed-size atomic budgets/counters change. No disk or network I/O runs here.
type Distributor struct {
	keys    map[uint32]*budget
	global  budget
	epoch   time.Time
	current func() *Object
	counts  *Counters
}

func NewDistributor(keys []uint32, current func() *Object, counts *Counters) *Distributor {
	d := &Distributor{keys: make(map[uint32]*budget, len(keys)), epoch: time.Now(), current: current, counts: counts}
	for _, id := range keys {
		if id != 0 {
			d.keys[id] = new(budget)
		}
	}
	return d
}

func (d *Distributor) Authorized(id uint32) bool { return d != nil && d.keys[id] != nil }

func (d *Distributor) Respond(req *Message, id uint32, wall, mono time.Time) (*Message, error) {
	if !d.Authorized(id) {
		return nil, Reject("auth", "key is not authorized to export leap data")
	}
	now := mono.Sub(d.epoch).Nanoseconds()
	if now < 0 || !d.keys[id].allow(now, 4*time.Second, 2) || !d.global.allow(now, time.Second/32, 32) {
		if d.counts != nil {
			d.counts.RateLimited.Add(1)
		}
		return nil, Reject("rate", "leap request budget exhausted")
	}
	var o *Object
	if d.current != nil {
		o = d.current()
	}
	if o != nil && (!wall.Before(o.table.Expiry) || wall.Before(o.table.Baseline)) {
		o = nil
	}
	r, err := Reply(req, o)
	if err == nil && d.counts != nil {
		d.counts.Served.Add(1)
	}
	return &r, err
}
