package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

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

// socketProbeTimeout bounds the connect used to tell a live socket from an
// abandoned one. A local unix socket with a listener accepts immediately.
const socketProbeTimeout = 500 * time.Millisecond

// Server answers control requests for one engine.
type Server struct {
	path    string
	ln      net.Listener
	lock    *os.File
	eng     *engine.Engine
	stats   *ntpserver.Stats
	version string
	log     *slog.Logger
	wg      sync.WaitGroup
}

// Listen creates the unix socket at path (mode 0660).
//
// Ownership of the path is taken with an exclusive advisory lock on a sibling
// lock file, held for the daemon's lifetime. That lock, not the presence of
// the socket inode, is what makes the daemon single-instance: the previous
// check-then-unlink sequence could remove a live socket and let a second
// listener bind the same name, giving the host two clock owners (RA6X-032).
//
// Only an abandoned socket is removed, and only while the lock is held. A
// path that is not a socket — a regular file, a directory, a symlink — is a
// configuration mistake and fails startup untouched, as does one that cannot
// be classified because the probe was refused permission or timed out.
func Listen(path string, eng *engine.Engine, stats *ntpserver.Stats, version string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	lock, err := lockPath(path)
	if err != nil {
		return nil, err
	}
	release := func() { _ = lock.Close() }
	if err := clearAbandonedSocket(path); err != nil {
		release()
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		release()
		return nil, fmt.Errorf("control: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		release()
		return nil, fmt.Errorf("control: chmod %s: %w", path, err)
	}
	return &Server{path: path, ln: ln, lock: lock, eng: eng, stats: stats, version: version, log: log}, nil
}

// lockPath takes the exclusive advisory lock that grants ownership of the
// control socket path. The lock file is a sibling of the socket and is left
// in place between runs: deleting it would reintroduce the race it exists to
// close, since a second starter could create and lock a fresh inode while the
// first still holds the old one.
func lockPath(path string) (*os.File, error) {
	name := path + ".lock"
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("control: opening lock file %s: %w", name, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrInUse, path)
		}
		return nil, fmt.Errorf("control: locking %s: %w", name, err)
	}
	return f, nil
}

// clearAbandonedSocket removes the socket at path if, and only if, it is a
// socket that nothing is listening on. The caller must already hold the path
// lock.
func clearAbandonedSocket(path string) error {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("control: inspecting %s: %w", path, err)
	}
	// Lstat, not Stat: a symlink is reported as a symlink and refused,
	// rather than being followed to whatever it points at and unlinked.
	if info.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("control: %s is %s, not a socket; refusing to replace it", path, describeMode(info.Mode()))
	}
	c, derr := net.DialTimeout("unix", path, socketProbeTimeout)
	if derr == nil {
		_ = c.Close()
		// We hold the lock, so this is not another carillon; something else
		// is listening on the daemon's control path.
		return fmt.Errorf("%w: %s", ErrInUse, path)
	}
	if !socketAbandoned(derr) {
		return fmt.Errorf("control: cannot tell whether %s is live; refusing to remove it: %w", path, derr)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("control: removing stale socket %s: %w", path, err)
	}
	return nil
}

// socketAbandoned reports whether a failed connect proves the socket has no
// listener. Only a refused connection does. A timeout means a full backlog or
// a wedged peer, and a permission error means the probe never reached the
// socket; in both cases the socket may well be live, and removing it would
// unlink a running daemon's inode.
func socketAbandoned(err error) bool {
	if errors.Is(err, fs.ErrNotExist) {
		// It went away between the Lstat and the dial; nothing to remove.
		return true
	}
	return errors.Is(err, unix.ECONNREFUSED)
}

// describeMode names a file type for an operator-facing error.
func describeMode(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "a directory"
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m.IsRegular():
		return "a regular file"
	case m&fs.ModeDevice != 0:
		return "a device node"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	default:
		return "not a socket"
	}
}

// Close releases the socket and the path lock without serving. It is safe to
// call more than once and after Serve has returned.
func (s *Server) Close() error {
	err := s.ln.Close()
	_ = os.Remove(s.path)
	s.releaseLock()
	return err
}

func (s *Server) releaseLock() {
	if s.lock != nil {
		// Closing the descriptor releases the flock.
		_ = s.lock.Close()
		s.lock = nil
	}
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
				s.releaseLock()
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			s.wg.Wait()
			_ = os.Remove(s.path)
			s.releaseLock()
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
