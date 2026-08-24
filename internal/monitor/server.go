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
	Log           *slog.Logger
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

	mux := http.NewServeMux()
	snapshot := func() Snapshot {
		stats := ntpserver.StatsSnapshot{}
		if cfg.Stats != nil {
			stats = cfg.Stats.Snapshot()
		}
		return SnapshotOf(cfg.Status(), stats, cfg.ServerEnabled, cfg.Metadata, cfg.Now())
	}
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, snapshot())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		s := snapshot()
		code := http.StatusOK
		if s.Health.Status != "healthy" {
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

// Close releases the listener. Serve normally owns shutdown; Close exists so
// startup can unwind cleanly if a later service fails to initialize.
func (s *Server) Close() error { return s.http.Close() }

// Serve handles requests until ctx is cancelled or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.log.Debug("monitor shutdown", "error", err)
		}
	})
	err := s.http.Serve(s.ln)
	stop()
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	if err != nil {
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

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
