package control

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestAstra6WaitDurationIsBounded covers the control-server half of
// RA6X-035. A request arrives as JSON on a local socket and need not come
// from carillonctl, so an out-of-range or non-finite timeout must not
// overflow the seconds-to-duration conversion into a deadline in the past.
func TestAstra6WaitDurationIsBounded(t *testing.T) {
	cases := []struct {
		name  string
		secs  float64
		want  time.Duration
		exact bool
	}{
		{"nan", math.NaN(), 0, true},
		{"negative", -1, 0, true},
		{"zero", 0, 0, true},
		{"+inf", math.Inf(1), time.Duration(maxWaitDuration), true},
		{"-inf", math.Inf(-1), 0, true},
		{"overflowing", 1e30, time.Duration(maxWaitDuration), true},
		{"one second", 1, time.Second, true},
		{"an hour", 3600, time.Hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := waitDuration(c.secs)
			if got < 0 {
				t.Fatalf("waitDuration(%v) = %v, which is a deadline in the past", c.secs, got)
			}
			if got > time.Duration(maxWaitDuration) {
				t.Fatalf("waitDuration(%v) = %v, above the bound", c.secs, got)
			}
			if c.exact && got != c.want {
				t.Fatalf("waitDuration(%v) = %v, want %v", c.secs, got, c.want)
			}
			// The server adds a reply allowance on top; that must not
			// overflow either.
			if got+replyWriteTimeout < 0 {
				t.Fatalf("waitDuration(%v) + reply allowance overflowed", c.secs)
			}
		})
	}
}

// TestAstra6ListenPreservesRegularFile is the review's RA6X-032 probe.
// Listen used to os.Stat the path without checking its type and remove
// anything that did not accept a connection within 500 ms, so a mistyped
// control path destroyed whatever regular file was there.
func TestAstra6ListenPreservesRegularFile(t *testing.T) {
	p := socketPath(t)
	if err := os.WriteFile(p, []byte("valuable contents"), 0600); err != nil {
		t.Fatal(err)
	}
	srv, err := Listen(p, newEngine(t), nil, "v", nil)
	if srv != nil {
		defer srv.Close()
	}
	b, readerr := os.ReadFile(p)
	if err == nil || readerr != nil || string(b) != "valuable contents" {
		t.Fatalf("existing file replaced: Listen=%v, read=%v, contents=%q", err, readerr, b)
	}
}

// TestAstra6ListenRefusesNonSockets covers the rest of RA6X-032's file-type
// list. None of these may be removed, and each failure must name the problem.
func TestAstra6ListenRefusesNonSockets(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		p := socketPath(t)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := Listen(p, newEngine(t), nil, "v", nil); err == nil {
			t.Fatal("a directory at the control path was accepted")
		}
		if info, err := os.Lstat(p); err != nil || !info.IsDir() {
			t.Fatalf("the directory was removed: %v %v", info, err)
		}
	})

	t.Run("symlink to a socket", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(dir, "r")
		link := filepath.Join(dir, "l")
		if len(link) > 100 {
			t.Skip("temp dir path too long for a unix socket")
		}
		ln, err := net.Listen("unix", real)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Listen(link, newEngine(t), nil, "v", nil); err == nil {
			t.Fatal("a symlink at the control path was accepted")
		}
		if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("the symlink was replaced: %v %v", info, err)
		}
		if _, err := os.Lstat(real); err != nil {
			t.Fatalf("the symlink's target was removed: %v", err)
		}
	})

	t.Run("named pipe", func(t *testing.T) {
		p := socketPath(t)
		if err := unix.Mkfifo(p, 0o600); err != nil {
			t.Skipf("cannot create a fifo here: %v", err)
		}
		if _, err := Listen(p, newEngine(t), nil, "v", nil); err == nil {
			t.Fatal("a named pipe at the control path was accepted")
		}
		if info, err := os.Lstat(p); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
			t.Fatalf("the fifo was removed: %v %v", info, err)
		}
	})
}

// TestAstra6ListenRefusesLiveSocket checks a busy listener is reported as
// in use and left intact, rather than unlinked so a second daemon can bind
// the same name and become a second clock owner.
func TestAstra6ListenRefusesLiveSocket(t *testing.T) {
	p := socketPath(t)
	first, err := Listen(p, newEngine(t), nil, "v", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- first.Serve(ctx) }()

	second, err := Listen(p, newEngine(t), nil, "v", nil)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("a second Listen on a live socket returned %v, want ErrInUse", err)
	}
	// The original listener must still answer.
	if _, err := Call(context.Background(), p, Request{Command: CmdVersion}); err != nil {
		t.Fatalf("the live listener stopped answering: %v", err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// TestAstra6ConcurrentStartersElectOneOwner checks the ownership lock:
// exactly one of several simultaneous starters may hold the control path.
func TestAstra6ConcurrentStartersElectOneOwner(t *testing.T) {
	p := socketPath(t)
	const starters = 8
	var wg sync.WaitGroup
	results := make(chan *Server, starters)
	errs := make(chan error, starters)
	for i := 0; i < starters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv, err := Listen(p, newEngine(t), nil, "v", nil)
			if err != nil {
				errs <- err
				return
			}
			results <- srv
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	var owners []*Server
	for srv := range results {
		owners = append(owners, srv)
	}
	for _, srv := range owners {
		defer srv.Close()
	}
	if len(owners) != 1 {
		t.Fatalf("%d of %d concurrent starters took the control path; want exactly 1", len(owners), starters)
	}
	for err := range errs {
		if !errors.Is(err, ErrInUse) {
			t.Fatalf("a losing starter returned %v, want ErrInUse", err)
		}
	}
}

// TestAstra6ListenRecoversAbandonedSocket keeps the behaviour the check
// exists for: a socket left by a crashed daemon is still cleaned up.
func TestAstra6ListenRecoversAbandonedSocket(t *testing.T) {
	p := socketPath(t)
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	srv, err := Listen(p, newEngine(t), nil, "v", nil)
	if err != nil {
		t.Fatalf("an abandoned socket must be recoverable: %v", err)
	}
	defer srv.Close()
	info, err := os.Lstat(p)
	if err != nil || info.Mode().Type() != fs.ModeSocket {
		t.Fatalf("no socket at the control path afterwards: %v %v", info, err)
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Fatalf("socket mode %o, want 660", perm)
	}
}

// TestAstra6SocketAbandonedClassification pins which dial failures may be
// read as "no listener". A timeout or a permission error must not be: both
// happen to live sockets, and removing one unlinks a running daemon.
func TestAstra6SocketAbandonedClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"refused", &net.OpError{Err: unix.ECONNREFUSED}, true},
		{"vanished", &net.OpError{Err: unix.ENOENT}, true},
		{"timeout", &net.OpError{Err: os.ErrDeadlineExceeded}, false},
		{"permission denied", &net.OpError{Err: unix.EACCES}, false},
		{"not permitted", &net.OpError{Err: unix.EPERM}, false},
		{"connection reset", &net.OpError{Err: unix.ECONNRESET}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := socketAbandoned(c.err); got != c.want {
				t.Fatalf("socketAbandoned(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestAstra6CallHonorsCancellation is the review's RA6X-033 probe.
// DialContext handles cancellation only while dialing; once connected, a
// cancellable context with no deadline did not interrupt a blocked read, so
// an indefinite waitsync hung after its caller had given up.
func TestAstra6CallHonorsCancellation(t *testing.T) {
	p := socketPath(t)
	ln, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := Call(ctx, p, Request{Command: CmdWaitSync}); done <- e }()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := make([]byte, 256)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err = c.Read(b); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Call returned %v, want a context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		c.Close()
		<-done
		t.Fatal("Call still blocked 2 s after context cancellation")
	}
}

// TestAstra6CallCancellationPoints covers the rest of RA6X-033's list.
func TestAstra6CallCancellationPoints(t *testing.T) {
	t.Run("before dial", func(t *testing.T) {
		p := socketPath(t)
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Call(ctx, p, Request{Command: CmdVersion}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Call returned %v, want a context.Canceled", err)
		}
	})

	t.Run("a server that never answers is bounded by the waitsync timeout", func(t *testing.T) {
		p := socketPath(t)
		ln, err := net.Listen("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Read the request and then say nothing, ever.
			b := make([]byte, 256)
			_, _ = c.Read(b)
			<-time.After(time.Minute)
			c.Close()
		}()
		start := time.Now()
		_, err = Call(context.Background(), p, Request{Command: CmdWaitSync, Timeout: 0.2})
		if err == nil {
			t.Fatal("a server that never answers must not succeed")
		}
		// The client's own deadline bounds it: the request timeout plus the
		// reply allowance, not ten seconds on top of it.
		if elapsed := time.Since(start); elapsed > 8*time.Second {
			t.Fatalf("Call took %v for a 0.2 s waitsync", elapsed)
		}
	})
}

// TestAstra6WaitSyncDisconnectDoesNotLeak covers RA6X-034. A zero-timeout
// waitsync cleared its deadlines and waited only on the daemon's context, so
// nothing noticed a client that had gone away and local clients accumulated
// for as long as the daemon stayed unsynchronized.
func TestAstra6WaitSyncDisconnectDoesNotLeak(t *testing.T) {
	p := socketPath(t)
	srv, err := Listen(p, newEngine(t), nil, "v", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()

	for i := 0; i < 20; i++ {
		conn, err := net.Dial("unix", p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write([]byte(`{"command":"waitsync","timeout":0}` + "\n")); err != nil {
			t.Fatal(err)
		}
		// Give the handler a moment to park in Engine.Wait, then vanish.
		time.Sleep(5 * time.Millisecond)
		conn.Close()
	}
	deadline := time.Now().Add(10 * time.Second)
	for srv.waiters.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiters still parked after their clients disconnected", srv.waiters.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// TestAstra6FatalAcceptDoesNotDeadlock covers the other half of RA6X-034: the
// fatal Accept branch waited for handlers whose context was not cancelled
// until the caller learned Serve had failed.
func TestAstra6FatalAcceptDoesNotDeadlock(t *testing.T) {
	p := socketPath(t)
	srv, err := Listen(p, newEngine(t), nil, "v", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()

	// A live waiter, parked indefinitely.
	conn, err := net.Dial("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(`{"command":"waitsync","timeout":0}` + "\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for srv.waiters.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the waiter never started")
		}
		time.Sleep(time.Millisecond)
	}

	// Close the listener under Serve: Accept fails for a reason that is not
	// cancellation, which is the fatal branch.
	_ = srv.ln.Close()
	select {
	case err := <-served:
		if err == nil {
			t.Fatal("a fatal Accept failure must be reported")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve deadlocked waiting for a handler it had not cancelled")
	}
	conn.Close()
}
