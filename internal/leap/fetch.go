package leap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
	"github.com/ptudor/carillon-time/internal/ntp/auth"
)

const NISTURL = "https://tf.nist.gov/leap-seconds.list"

// FetchNIST uses the system trust store and certificate date validation.
// Neither a peer nor configuration can replace the URL or TLS policy.
func FetchNIST(ctx context.Context) (*Object, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // acquisition policy never comes from the environment
	transport.DisableCompression = true
	defer transport.CloseIdleConnections()
	return fetchHTTPS(ctx, &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("leap: redirects are forbidden") }}, NISTURL)
}

// The injected client/URL are private test seams. Production only calls the
// fixed-URL entry point above, after establishing UTC.
func fetchHTTPS(ctx context.Context, client *http.Client, url string) (*Object, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" {
		return nil, Reject("transport", "HTTPS is required")
	}
	req.Header.Set("Accept-Encoding", "identity")
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("leap: NIST fetch: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, Reject("http", fmt.Sprintf("HTTP status %d", res.StatusCode))
	}
	if res.Header.Get("Content-Encoding") != "" && res.Header.Get("Content-Encoding") != "identity" {
		return nil, Reject("transport", "encoded body refused")
	}
	if res.ContentLength > MaxFileSize {
		return nil, Reject("size", "HTTP body exceeds 64 KiB")
	}
	return ReadObject(res.Body)
}

// Peer is an explicitly trusted configured association. It has its own UDP
// socket and never emits a time measurement or updates a source's reach.
type Peer struct {
	Name    string
	Address string // already validated host:port
	Key     auth.Key
	// Busy reports an ordinary time exchange in progress. It only delays
	// this worker; the leap learner cannot alter time polling.
	Busy func() bool
}

type PeerError struct {
	Reason      string
	RetryAfter  time.Duration
	MinInterval time.Duration
	Stop        bool
}

func (e *PeerError) Error() string { return "leap peer: " + e.Reason }

type peerClient struct {
	conn    net.Conn
	peer    Peer
	next    time.Time
	spacing time.Duration
	timeout time.Duration
	probes  func()
	bytes   func(uint64)
}

func (p *peerClient) exchange(ctx context.Context, request Message) (*Message, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := pause(ctx, time.Until(p.next)); err != nil {
			return nil, err
		}
		for p.peer.Busy != nil && p.peer.Busy() {
			if err := pause(ctx, 250*time.Millisecond); err != nil {
				return nil, err
			}
		}
		if _, err := rand.Read(request.ID[:]); err != nil {
			return nil, err
		}
		if request.ID == ([16]byte{}) {
			request.ID[0] = 1
		}
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		xmit := ntp.Time(binary.BigEndian.Uint64(nonce[:]))
		if xmit == 0 {
			xmit = 1
		}
		header := ntp.Packet{Version: 4, Mode: ntp.ModeClient, Leap: ntp.LeapUnsync, Poll: 2, TransmitTime: xmit}
		buf, err := request.AppendTo(header.Marshal())
		if err != nil {
			return nil, err
		}
		buf = p.peer.Key.Append(buf)
		deadline := time.Now().Add(p.timeout)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := p.conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
		p.next = time.Now().Add(p.spacing * time.Duration(1<<attempt))
		if _, err := p.conn.Write(buf); err != nil {
			return nil, err
		}
		if request.Operation == Probe && p.probes != nil {
			p.probes()
		}
		var received [ntp.MaxPacketSize + 1]byte
		for {
			n, err := p.conn.Read(received[:])
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break
				}
				return nil, err
			}
			if n > ntp.MaxPacketSize {
				continue
			}
			m, err := checkReply(received[:n], p.peer.Key, xmit, &request)
			if err != nil {
				var pe *PeerError
				if errors.As(err, &pe) {
					return nil, err
				}
				continue // forged, reflected, stale and unsolicited packets
			}
			if p.bytes != nil {
				p.bytes(uint64(len(m.Data)))
			}
			// The next read reuses received. Return owned, bounded bytes.
			m.Data = append([]byte(nil), m.Data...)
			return m, nil
		}
	}
	if request.Operation == Probe {
		return nil, Reject("timeout", "probe timed out after three attempts")
	}
	return nil, Reject("timeout", "chunk timed out after three attempts")
}

func checkReply(buf []byte, key auth.Key, origin ntp.Time, req *Message) (*Message, error) {
	p, mac, offset, err := ntp.Decode(buf)
	if err != nil || mac == nil || !key.Verify(buf[:offset], mac) {
		return nil, Reject("auth", "response authentication failed")
	}
	if p.Version != 4 || p.Mode != ntp.ModeServer || p.OriginTime != origin {
		return nil, Reject("correlation", "response header does not match request")
	}
	if p.IsKiss() {
		code := p.KissCode()
		wait := 24 * time.Hour
		if code == "RATE" {
			poll := min(max(p.Poll, 2), 17)
			wait = time.Duration(ntp.Log2Seconds(poll) * float64(time.Second))
		}
		pe := &PeerError{Reason: "kiss " + code, RetryAfter: wait, Stop: code == "DENY" || code == "RSTR"}
		if code == "RATE" {
			pe.MinInterval = wait
		}
		return nil, pe
	}
	m, err := DecodeFields(buf[ntp.HeaderSize:offset])
	if errors.Is(err, ErrUnsupported) || (err == nil && m == nil) {
		return nil, &PeerError{Reason: "unsupported", RetryAfter: 24 * time.Hour}
	}
	if err != nil {
		return nil, err
	}
	if m.ID != req.ID || m.Operation != req.Operation+1 {
		return nil, Reject("correlation", "response ID or operation mismatch")
	}
	if req.Operation == Get && (m.Manifest != req.Manifest || m.Offset != req.Offset) {
		return nil, Reject("correlation", "chunk identifies another object or offset")
	}
	return m, nil
}

func (p Peer) fetch(ctx context.Context, cached *Object, now time.Time, count *Counters, spacing time.Duration) (*Object, error) {
	if p.Key.ID == 0 || len(p.Key.Secret) != 16 {
		return nil, Reject("auth", "peer requires a CMAC key")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "udp", p.Address)
	if err != nil {
		return nil, fmt.Errorf("leap peer %s: %w", p.Name, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	c := peerClient{conn: conn, peer: p, spacing: spacing, timeout: 4 * time.Second,
		probes: func() { count.Probes.Add(1) }, bytes: func(n uint64) { count.Bytes.Add(n) }}
	return c.fetch(ctx, cached, now)
}

func (c *peerClient) fetch(ctx context.Context, cached *Object, now time.Time) (*Object, error) {
	req := Message{Operation: Probe}
	if cached != nil {
		req.Manifest = cached.manifest
	}
	manifest, err := c.exchange(ctx, req)
	if err != nil {
		return nil, err
	}
	// A successful authenticated probe is recovery evidence after RATE.
	// The scheduler has already honored the kiss's retry floor. Resume the
	// normal request interval so a single old kiss cannot strand a large
	// object behind the overall transfer deadline indefinitely.
	c.spacing = min(c.spacing, 4*time.Second)
	c.next = time.Now().Add(c.spacing)
	if manifest.Result == NoTable {
		return nil, Reject("missing", "distributor has no current table")
	}
	if err := CheckManifest(cached, manifest.Manifest, now); err != nil {
		return nil, err
	}
	if cached != nil && manifest.Manifest == cached.manifest {
		return nil, nil
	}
	chunks := (manifest.Manifest.Size + ChunkSize - 1) / ChunkSize
	if time.Duration(chunks)*c.spacing >= 30*time.Minute {
		return nil, Reject("rate", "peer RATE interval cannot fit this object in the 30-minute transfer bound")
	}
	data := make([]byte, 0, manifest.Manifest.Size)
	for offset := uint32(0); offset < manifest.Manifest.Size; offset += ChunkSize {
		chunk, err := c.exchange(ctx, Message{Operation: Get, Manifest: manifest.Manifest, Offset: offset, Count: ChunkSize})
		if err != nil {
			return nil, err
		}
		if chunk.Result != OK {
			return nil, Reject("changed", "distributor no longer holds the requested object")
		}
		data = append(data, chunk.Data...)
	}
	o, err := NewObject(data)
	if err != nil {
		return nil, err
	}
	if o.manifest != manifest.Manifest {
		return nil, Reject("digest", "assembled file does not match manifest")
	}
	return o, nil
}

func CheckManifest(old *Object, m Manifest, now time.Time) error {
	if err := m.Validate(); err != nil {
		return Reject("metadata", err.Error())
	}
	if now.IsZero() {
		return Reject("time_unknown", "UTC has not been established")
	}
	u, _ := Date(m.Updated)
	e, _ := Date(m.Expires)
	if !now.Before(e) {
		return Reject("expired", "advertised table has expired")
	}
	if u.After(now.Add(5*time.Minute)) || e.After(now.Add(400*24*time.Hour)) {
		return Reject("date", "advertised dates are outside UTC bounds")
	}
	if old != nil && m != old.manifest {
		if m.Updated < old.manifest.Updated || m.Expires < old.manifest.Expires {
			return Reject("rollback", "advertised dates decreased")
		}
		if m.Updated == old.manifest.Updated && m.Expires == old.manifest.Expires {
			return Reject("conflict", "different bytes with identical advertised dates")
		}
	}
	return nil
}

func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
