package leap

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
	"github.com/ptudor/carillon-time/internal/ntp/auth"
)

func transferKey() auth.Key { return auth.Key{ID: 1, Secret: []byte("0123456789abcdef")} }

func authenticatedReply(t *testing.T, req Message, response Message, key auth.Key, origin ntp.Time) []byte {
	t.Helper()
	header := ntp.Packet{Version: 4, Mode: ntp.ModeServer, Leap: ntp.LeapUnsync, Stratum: 16, OriginTime: origin}
	b, err := response.AppendTo(header.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	return key.Append(b)
}

func TestReplyAuthenticationAndCorrelation(t *testing.T) {
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	key := transferKey()
	req := Message{Operation: Get, ID: [16]byte{1}, Manifest: o.manifest, Count: ChunkSize}
	response, err := Reply(&req, o)
	if err != nil {
		t.Fatal(err)
	}
	b := authenticatedReply(t, req, response, key, 123)
	if _, err := checkReply(b, key, 123, &req); err != nil {
		t.Fatal(err)
	} // LI=3 is deliberately allowed
	for i := range b {
		bad := bytes.Clone(b)
		bad[i] ^= 1
		if _, err := checkReply(bad, key, 123, &req); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
	for _, mutate := range []func(*Message){
		func(m *Message) { m.ID[0]++ },
		func(m *Message) { m.Manifest.Digest[0]++ },
	} {
		other := req
		mutate(&other)
		if _, err := checkReply(b, key, 123, &other); err == nil {
			t.Fatal("uncorrelated chunk accepted")
		}
	}
	if _, err := checkReply(b, key, 124, &req); err == nil {
		t.Fatal("previous exchange replay accepted")
	}
	if _, err := checkReply(b[:len(b)-20], key, 123, &req); err == nil {
		t.Fatal("unsigned data accepted")
	}
	reflected, _ := req.AppendTo((&ntp.Packet{Version: 4, Mode: ntp.ModeClient, OriginTime: 123}).Marshal())
	if _, err := checkReply(key.Append(reflected), key, 123, &req); err == nil {
		t.Fatal("request reflection accepted")
	}
}

func TestLegacyAndKissResponses(t *testing.T) {
	key := transferKey()
	req := Message{Operation: Probe, ID: [16]byte{1}}
	p := ntp.Packet{Version: 4, Mode: ntp.ModeServer, Stratum: 2, OriginTime: 7}
	_, err := checkReply(key.Append(p.Marshal()), key, 7, &req)
	var pe *PeerError
	if !errors.As(err, &pe) || pe.RetryAfter < 24*time.Hour {
		t.Fatalf("legacy backoff: %v", err)
	}
	for _, code := range []string{"RATE", "DENY", "RSTR"} {
		p.Stratum = 0
		p.ReferenceID = ntp.RefID{code[0], code[1], code[2], code[3]}
		p.Poll = 17
		_, err := checkReply(key.Append(p.Marshal()), key, 7, &req)
		if !errors.As(err, &pe) {
			t.Fatalf("KoD accepted: %v", err)
		}
		if code == "RATE" && pe.RetryAfter < 131072*time.Second {
			t.Fatal("RATE interval capped too low")
		}
		if code != "RATE" && !pe.Stop {
			t.Fatal("authenticated denial did not stop learner")
		}
	}
}

// wirePeer exercises the complete authenticated UDP learner using the shared
// protocol responder. The listener package separately tests its ACL/budgets.
func wirePeer(t *testing.T, current func() *Object, change func(*Message)) Peer {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [1501]byte
		for {
			n, addr, err := c.ReadFrom(b[:])
			if err != nil {
				return
			}
			pkt, mac, offset, err := ntp.Decode(b[:n])
			if err != nil || !transferKey().Verify(b[:offset], mac) {
				continue
			}
			req, err := DecodeFields(b[48:offset])
			if err != nil || req == nil {
				continue
			}
			res, err := Reply(req, current())
			if err != nil {
				continue
			}
			if change != nil {
				change(&res)
			}
			header := ntp.Packet{Version: 4, Mode: ntp.ModeServer, Leap: ntp.LeapUnsync, Stratum: 16, OriginTime: pkt.TransmitTime}
			out, err := res.AppendTo(header.Marshal())
			if err != nil {
				continue
			}
			c.WriteTo(transferKey().Append(out), addr)
		}
	}()
	t.Cleanup(func() { c.Close(); <-done })
	return Peer{Name: "relay", Address: c.LocalAddr().String(), Key: transferKey()}
}

type testController struct {
	view          atomic.Pointer[View]
	active        atomic.Pointer[Record]
	activateError error
	checkCommit   func(*Record) error
}

func (c *testController) LeapView() View { return *c.view.Load() }
func (c *testController) ApproveLeap(_ context.Context, r *Record) error {
	v := c.LeapView()
	if !v.Known {
		return Reject("time_unknown", "test UTC unknown")
	}
	now := v.Now
	if v.UTCbound.After(now) {
		now = v.UTCbound
	}
	var old *Object
	if a := c.active.Load(); a != nil {
		old = a.Object
	}
	return CheckUpdate(old, r.Object, now)
}
func (c *testController) ActivateLeap(ctx context.Context, r *Record) error {
	if c.activateError != nil {
		return c.activateError
	}
	if c.checkCommit != nil {
		if err := c.checkCommit(r); err != nil {
			return err
		}
	}
	if err := c.ApproveLeap(ctx, r); err != nil {
		return err
	}
	c.active.Store(r)
	return nil
}

func testUpdater(t *testing.T, mode string, now time.Time) (*Updater, *testController) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "leap"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := new(testController)
	c.view.Store(&View{Now: now, Known: true, UTCbound: now})
	c.checkCommit = func(r *Record) error {
		state, err := s.Load()
		if err != nil {
			return err
		}
		if state.Pending == nil || state.Pending.Object.manifest != r.Object.manifest {
			return errors.New("activation preceded durable commit")
		}
		return nil
	}
	u := NewUpdater(UpdaterConfig{Mode: mode, Store: s, Controller: c, Report: new(Report)})
	return u, c
}

func TestHTTPSSeedRelayChainAndRenewal(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	old := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "# first publication\n")
	renew := objectForTest(t, "2016-07-08T00:00:00Z", "2027-06-28T00:00:00Z", "# expiry-only renewal\n")
	var published atomic.Pointer[Object]
	published.Store(old)
	var downloads atomic.Int32
	https := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("identity encoding not requested")
		}
		downloads.Add(1)
		w.Write(published.Load().Bytes())
	}))
	defer https.Close()
	client := https.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	seed, sc := testUpdater(t, "nist", now)
	relay, rc := testUpdater(t, "peers", now)
	leaf, lc := testUpdater(t, "peers", now)
	// Downstreams cannot fetch HTTPS, even through an accidental fallback.
	relay.fetchNIST = func(context.Context) (*Object, error) {
		t.Error("relay fetched HTTPS")
		return nil, errors.New("egress denied")
	}
	leaf.fetchNIST = relay.fetchNIST
	fromSeed := wirePeer(t, func() *Object {
		if r := sc.active.Load(); r != nil {
			return r.Object
		}
		return nil
	}, nil)
	fromRelay := wirePeer(t, func() *Object {
		if r := rc.active.Load(); r != nil {
			return r.Object
		}
		return nil
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i, want := range []*Object{old, renew} {
		published.Store(want)
		o, err := fetchHTTPS(ctx, client, https.URL)
		if err != nil {
			t.Fatal(err)
		}
		if err := seed.install(ctx, o, Provider{Kind: "nist", Name: NISTURL}); err != nil {
			t.Fatal(err)
		}
		for _, hop := range []struct {
			peer    Peer
			updater *Updater
			control *testController
		}{{fromSeed, relay, rc}, {fromRelay, leaf, lc}} {
			o, err := hop.peer.fetch(ctx, hop.updater.state.Anchor(), now, &hop.updater.cfg.Report.Counters, time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			if err := hop.updater.install(ctx, o, Provider{Kind: "peer", Name: hop.peer.Name, KeyID: 1}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(hop.control.active.Load().Object.Bytes(), want.Bytes()) {
				t.Fatal("relay changed original bytes")
			}
			state, err := hop.updater.cfg.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if state.Active.Object.manifest != want.manifest || state.Pending != nil {
				t.Fatal("restart lost active generation")
			}
			if i == 1 && state.Active.Object.manifest.Updated != old.manifest.Updated {
				t.Fatal("expiry renewal redated leap records")
			}
		}
	}
	if downloads.Load() != 2 {
		t.Fatalf("downloads: %d, want only the seed's two checks", downloads.Load())
	}
	disk, err := leaf.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewUpdater(UpdaterConfig{Mode: "peers", Store: leaf.cfg.Store, Initial: disk, Controller: lc})
	if err := restarted.install(ctx, old, Provider{Kind: "peer", Name: "replay", KeyID: 1}); Reason(err) != "rollback" {
		t.Fatalf("post-restart rollback: %v", err)
	}
	seed.reject(errors.New("NIST unavailable"), nil)
	if sc.active.Load().Object.manifest != renew.manifest {
		t.Fatal("fetch failure withdrew active table")
	}
}

func TestTransferRejectsChangedObjectAndForgedDigest(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "# comment\n")
	for _, kind := range []string{"changed", "digest"} {
		t.Run(kind, func(t *testing.T) {
			p := wirePeer(t, func() *Object { return o }, func(m *Message) {
				if m.Operation != Data {
					return
				}
				if kind == "changed" {
					m.Result = NotFound
					m.Count = 0
					m.Data = nil
				} else {
					m.Data[len(m.Data)-2] = '!'
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if got, err := p.fetch(ctx, nil, now, new(Counters), time.Millisecond); Reason(err) != kind || got != nil {
				t.Fatalf("poisoned transfer: %v", err)
			}
		})
	}
}

func TestHTTPSPolicy(t *testing.T) {
	for _, kind := range []string{"redirect", "oversize", "encoding", "not_modified", "untrusted_tls"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					w.Header().Set("Location", "http://127.0.0.1/")
					w.WriteHeader(302)
				case "oversize":
					w.Write(make([]byte, MaxFileSize+1))
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
					w.Write([]byte("x"))
				case "not_modified":
					w.WriteHeader(304)
				}
			}))
			defer srv.Close()
			client := srv.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
			if kind == "untrusted_tls" {
				client = &http.Client{Timeout: time.Second}
			}
			if o, err := fetchHTTPS(context.Background(), client, srv.URL); err == nil || o != nil {
				t.Fatal("unsafe HTTPS result accepted")
			}
		})
	}
	if _, err := fetchHTTPS(context.Background(), http.DefaultClient, "http://127.0.0.1"); err == nil {
		t.Fatal("HTTP downgrade accepted")
	}
}

func TestCommitFailureAndActivationRecheck(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", "")
	n := objectForTest(t, "2016-07-08T00:00:00Z", "2027-06-28T00:00:00Z", "")
	u, c := testUpdater(t, "manual", now)
	if err := u.install(context.Background(), o, Provider{Kind: "file", Name: "local"}); err != nil {
		t.Fatal(err)
	}
	u.cfg.Store.fault = func(stage string) error {
		if stage == "directory_sync" {
			return errors.New("disk fault")
		}
		return nil
	}
	if err := u.install(context.Background(), n, Provider{Kind: "file", Name: "local"}); err == nil {
		t.Fatal("failed fsync accepted")
	}
	if c.active.Load().Object.manifest != o.manifest {
		t.Fatal("failed commit activated")
	}
	if u.state.Anchor().manifest != n.manifest {
		t.Fatal("ambiguous rename lowered rollback anchor")
	}
	u.cfg.Store.fault = nil
	if err := u.save(); err != nil {
		t.Fatal(err)
	}
	c.activateError = Reject("armed", "boundary changed between approval and activation")
	if err := u.activate(context.Background()); Reason(err) != "armed" {
		t.Fatal(err)
	}
	restarted, err := u.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Active.Object.manifest != o.manifest || restarted.Pending.Object.manifest != n.manifest {
		t.Fatal("failed activation discarded last usable generation")
	}
	c.activateError = nil
	c.view.Store(&View{Now: n.table.Expiry, Known: true, UTCbound: n.table.Expiry})
	if err := u.activate(context.Background()); Reason(err) != "expired" {
		t.Fatalf("expiry racing activation: %v", err)
	}
	if c.active.Load().Object.manifest != o.manifest {
		t.Fatal("expired candidate activated")
	}
}

func TestProbeTimeoutIsBoundedAndCancels(t *testing.T) {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	conn, err := net.Dial("udp", c.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	p := peerClient{conn: conn, peer: Peer{Key: transferKey()}, spacing: time.Millisecond, timeout: time.Millisecond}
	_, err = p.exchange(context.Background(), Message{Operation: Probe})
	var pe *PeerError
	if Reason(err) != "timeout" || errors.As(err, &pe) {
		t.Fatalf("timeout backoff: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.exchange(ctx, Message{Operation: Probe}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe: %v", err)
	}
}
