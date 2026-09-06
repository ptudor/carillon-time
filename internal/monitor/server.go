package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"time"

	"carillon/internal/engine"
	ntpserver "carillon/internal/server"
)

// Config configures the monitoring HTTP service.
type Config struct {
	Listen        netip.AddrPort
	Allow         []netip.Prefix
	Metadata      Metadata
	ServerEnabled bool
	Status        func() *engine.Status
	Stats         *ntpserver.Stats
	Now           func() time.Time

	// Monotonic reads the same monotonic scale the engine stamps its
	// snapshots with, so freshness is measured by elapsed time rather than
	// by subtracting two wall-clock readings a step may have moved apart
	// (RA6X-053).
	Monotonic func() float64

	Log *slog.Logger
}

// Server owns a bound TCP listener and its HTTP server.
type Server struct {
	ln   net.Listener
	http *http.Server
	log  *slog.Logger
}

// Listen validates cfg, binds the TCP listener, and prepares all handlers.
// Binding before the engine starts turns an address conflict into a startup
// failure rather than a silently missing monitoring surface.
func Listen(cfg Config) (*Server, error) {
	if !cfg.Listen.IsValid() {
		return nil, errors.New("monitor: invalid listen address")
	}
	if len(cfg.Allow) == 0 {
		return nil, errors.New("monitor: allow must contain at least one prefix")
	}
	if cfg.Status == nil {
		return nil, errors.New("monitor: nil status provider")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Monotonic == nil {
		start := time.Now()
		cfg.Monotonic = func() float64 { return time.Since(start).Seconds() }
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	allow := make([]netip.Prefix, len(cfg.Allow))
	for i, p := range cfg.Allow {
		if !p.IsValid() {
			return nil, fmt.Errorf("monitor: invalid allow prefix at index %d", i)
		}
		allow[i] = p.Masked()
	}

	network := "tcp6"
	addr := cfg.Listen
	if addr.Addr().Unmap().Is4() {
		network = "tcp4"
		addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	}
	ln, err := net.ListenTCP(network, net.TCPAddrFromAddrPort(addr))
	if err != nil {
		return nil, fmt.Errorf("monitor: listen %s: %w", addr, err)
	}

	// Process identity is captured once, here, so it cannot move when the
	// clock is stepped (RA6X-053).
	startedAt := cfg.Now()

	mux := http.NewServeMux()
	snapshot := func() Snapshot {
		stats := ntpserver.StatsSnapshot{}
		if cfg.Stats != nil {
			stats = cfg.Stats.Snapshot()
		}
		return SnapshotOf(cfg.Status(), stats, cfg.ServerEnabled, cfg.Metadata, cfg.Now(), cfg.Monotonic(), startedAt)
	}
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, snapshot())
	})
	// Only unhealthy is 503. A degraded instance — holdover, a lost preferred
	// source, an expiring leapfile — is still serving time worth using, and
	// failing its probe would take a working server out of a load balancer or
	// raise a page for a condition that needs no immediate action. The
	// distinction stays in the body for a client that wants to alert on it.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		s := snapshot()
		code := http.StatusOK
		if s.Health.Status == statusUnhealthy {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, struct {
			Schema     string    `json:"schema"`
			SnapshotAt time.Time `json:"snapshot_at"`
			Health     Health    `json:"health"`
		}{Schema: s.Schema, SnapshotAt: s.SnapshotAt, Health: s.Health})
	})
	mux.Handle("GET /metrics", newMetricsHandler(snapshot))

	h := securityHeaders(allowPeers(allow, mux))
	httpServer := &http.Server{
		Handler:           h,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	return &Server{ln: ln, http: httpServer, log: cfg.Log}, nil
}

// Addr returns the actual bound address, including an ephemeral test port.
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close releases the listener and any connections. It is safe to call more
// than once, before Serve, and after Serve has returned.
//
// http.Server.Close only closes listeners it has been given, and Serve is
// what gives it this one — so closing before Serve left the port bound and a
// startup that unwound after binding the monitor could not be retried
// (RA6X-052). The listener is closed here directly.
func (s *Server) Close() error {
	err := s.http.Close()
	if lerr := s.ln.Close(); lerr != nil && !errors.Is(lerr, net.ErrClosed) && err == nil {
		err = lerr
	}
	return err
}

// Serve handles requests until ctx is cancelled or the listener fails.
//
// It does not return until shutdown has actually completed: the cancellation
// callback runs asynchronously, so Serve used to return as soon as
// http.Serve did, while handlers were still draining and a shutdown timeout
// was logged without anything being forced closed. Callers can now rely on
// the advertised lifecycle being finished.
func (s *Server) Serve(ctx context.Context) error {
	shutdownDone := make(chan struct{})
	var shutdownOnce sync.Once
	stop := context.AfterFunc(ctx, func() {
		defer close(shutdownDone)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), monitorShutdownGrace)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			// The graceful deadline passed with connections still open.
			// Force them closed rather than logging and leaving them.
			s.log.Debug("monitor shutdown", "error", err)
			shutdownOnce.Do(func() { _ = s.Close() })
		}
	})
	err := s.http.Serve(s.ln)
	if !stop() {
		// The callback was already running: join it before reporting that
		// Serve has finished.
		<-shutdownDone
	}
	_ = s.ln.Close()
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("monitor: serve: %w", err)
	}
	return nil
}

func allowPeers(allow []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		addr := peer.Addr().Unmap().WithZone("")
		if !slices.ContainsFunc(allow, func(p netip.Prefix) bool { return p.Contains(addr) }) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// monitorShutdownGrace is how long in-flight requests have to finish before
// their connections are forced closed.
const monitorShutdownGrace = 5 * time.Second

// writeJSON serializes value and only then commits a status code, so an
// encoding failure becomes a visible 500 rather than a 200 with an empty
// body. Non-finite numbers are the realistic cause, and reporting them as a
// successful empty response hid exactly the state an operator needed to see
// (RA6X-049).
func writeJSON(w http.ResponseWriter, code int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"error":"internal encoding failure"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(append(body, '\n'))
}
