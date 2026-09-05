package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"carillon/internal/discipline"
	"carillon/internal/engine"
	ntpserver "carillon/internal/server"
)

// maxRequest bounds a request line; anything larger is not a request.
const maxRequest = 4096

// replyWriteTimeout bounds how long a reply may take to reach a client that
// has stopped reading, so one such client cannot hold shutdown open.
const replyWriteTimeout = 5 * time.Second

// maxWaitDuration is the longest waitsync a request may ask for. It leaves
// room for the server's and the client's extra reply allowances to be added
// without overflowing time.Duration,
// and is about 292 years, so it bounds nothing an operator would type.
const maxWaitDuration = math.MaxInt64 - int64(4*replyWriteTimeout)

// waitDuration converts a request's timeout in seconds to a bounded
// time.Duration. Non-finite and out-of-range values saturate rather than
// wrapping to a negative deadline that would expire immediately.
func waitDuration(secs float64) time.Duration {
	if math.IsNaN(secs) || secs <= 0 {
		return 0
	}
	ns := secs * float64(time.Second)
	if math.IsInf(ns, 1) || ns >= float64(maxWaitDuration) {
		return time.Duration(maxWaitDuration)
	}
	return time.Duration(ns)
}

// ErrInUse is returned by Listen when another daemon holds the socket.
var ErrInUse = errors.New("control: socket is in use by another carillon")

// Server answers control requests for one engine.
type Server struct {
	path    string
	ln      net.Listener
	eng     *engine.Engine
	stats   *ntpserver.Stats
	version string
	log     *slog.Logger
	wg      sync.WaitGroup
}

// Listen creates the unix socket at path (mode 0660). A stale socket file
// left by a crashed daemon is removed; one that still answers is an error.
func Listen(path string, eng *engine.Engine, stats *ntpserver.Stats, version string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if _, err := os.Stat(path); err == nil {
		c, derr := net.DialTimeout("unix", path, 500*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("%w: %s", ErrInUse, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("control: removing stale socket %s: %w", path, err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("control: chmod %s: %w", path, err)
	}
	return &Server{path: path, ln: ln, eng: eng, stats: stats, version: version, log: log}, nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until ctx is done, then closes the listener,
// waits for in-flight requests, and removes the socket file.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				s.wg.Wait()
				_ = os.Remove(s.path)
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.wg.Wait()
			_ = os.Remove(s.path)
			return fmt.Errorf("control: accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReaderSize(conn, maxRequest)
	line, err := r.ReadSlice('\n')
	if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
		s.reply(conn, Response{Error: "control: malformed request: " + err.Error()})
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.reply(conn, Response{Error: "control: malformed request: " + err.Error()})
		return
	}
	s.reply(conn, s.dispatch(ctx, conn, req))
}

func (s *Server) dispatch(ctx context.Context, conn net.Conn, req Request) Response {
	switch req.Command {
	case CmdVersion:
		return Response{Version: s.version}
	case CmdTracking:
		return Response{Tracking: TrackingOf(s.eng.Status())}
	case CmdSources:
		return Response{Sources: SourcesOf(s.eng.Status())}
	case CmdRefclock:
		return Response{Refclocks: RefclocksOf(s.eng.Status())}
	case CmdServerStats:
		return Response{ServerStats: ServerStatsOf(s.stats.Snapshot())}
	case CmdWaitSync:
		wctx := ctx
		var cancel context.CancelFunc
		if req.Timeout > 0 {
			// A request is JSON off a local socket, not necessarily from
			// carillonctl: an out-of-range or non-finite timeout must not
			// overflow the conversion into a negative deadline (RA6X-035).
			wctx, cancel = context.WithTimeout(ctx, waitDuration(req.Timeout))
			defer cancel()
			_ = conn.SetDeadline(time.Now().Add(waitDuration(req.Timeout) + replyWriteTimeout))
		} else {
			_ = conn.SetDeadline(time.Time{})
		}
		err := s.eng.Wait(wctx, func(st *engine.Status) bool { return st.State == discipline.StateSynced })
		synced := err == nil
		return Response{Synced: &synced}
	default:
		return Response{Error: fmt.Sprintf("control: unknown command %q", req.Command)}
	}
}

func (s *Server) reply(conn net.Conn, resp Response) {
	b, err := json.Marshal(resp)
	if err != nil {
		s.log.Error("control: encoding response", "error", err)
		return
	}
	b = append(b, '\n')
	// A write deadline of its own. A waitsync with no timeout clears the
	// connection's deadline entirely, so a client that asked for one and
	// then stopped reading would hold this handler — and the daemon's
	// shutdown wait — open indefinitely once the socket buffer filled.
	_ = conn.SetWriteDeadline(time.Now().Add(replyWriteTimeout))
	if _, err := conn.Write(b); err != nil {
		s.log.Debug("control: writing response", "error", err)
	}
}
