package leap

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ptudor/carillon-time/internal/ntp"
)

func TestNISTFetchIgnoresProxyEnvironment(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		t.Setenv(name, "http://127.0.0.1:1")
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	// Stop at DialContext so this tests the real fixed-URL entry point
	// without consulting DNS or making any external connection.
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	// Avoid ProxyFromEnvironment's process-wide cache from earlier tests.
	transport.Proxy = func(*http.Request) (*url.URL, error) { return url.Parse(os.Getenv("HTTPS_PROXY")) }
	dialed := make(chan string, 1)
	stop := errors.New("test stopped before network I/O")
	transport.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed <- address
		return nil, stop
	}
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = original; transport.CloseIdleConnections() })
	if _, err := FetchNIST(context.Background()); !errors.Is(err, stop) {
		t.Fatalf("unexpected fetch result: %v", err)
	}
	if address := <-dialed; address != "tf.nist.gov:443" {
		t.Fatalf("environment redirected the fixed-URL fetch to %q", address)
	}
}

func TestSuccessfulProbeRecoversRATEForLargeObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
		o := objectForTest(t, "2016-07-08T00:00:00Z", "2026-12-28T00:00:00Z", strings.Repeat("# padding\n", 1200))
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()
		go func() {
			defer server.Close()
			for {
				var b [ntp.MaxPacketSize]byte
				n, err := server.Read(b[:])
				if err != nil {
					return
				}
				pkt, mac, end, err := ntp.Decode(b[:n])
				if err != nil || !transferKey().Verify(b[:end], mac) {
					t.Error("invalid learner request")
					return
				}
				req, err := DecodeFields(b[ntp.HeaderSize:end])
				if err != nil || req == nil {
					t.Errorf("invalid CLPS request: %v", err)
					return
				}
				res, err := Reply(req, o)
				if err != nil {
					t.Error(err)
					return
				}
				out := authenticatedReply(t, *req, res, transferKey(), pkt.TransmitTime)
				if _, err := server.Write(out); err != nil && !errors.Is(err, io.ErrClosedPipe) {
					t.Error(err)
					return
				}
			}
		}()
		c := peerClient{conn: client, peer: Peer{Key: transferKey()}, spacing: 64 * time.Second, timeout: 4 * time.Second}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		start := time.Now()
		got, err := c.fetch(ctx, nil, now)
		if err != nil || got == nil || got.manifest != o.manifest {
			t.Fatalf("successful probe did not restore large-file transfer: %v", err)
		}
		chunks := (o.manifest.Size + ChunkSize - 1) / ChunkSize
		if elapsed := time.Since(start); elapsed != time.Duration(chunks)*4*time.Second {
			t.Fatalf("transfer interval: %s for %d chunks", elapsed, chunks)
		}
	})
}
