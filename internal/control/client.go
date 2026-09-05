package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"syscall"
	"time"
)

const (
	// maxResponse bounds a response line.
	maxResponse = 1 << 20

	// connectRetry is how long WaitSync pauses between attempts to reach a
	// daemon that is still starting.
	connectRetry = 250 * time.Millisecond
)

// Call sends one request to the daemon at path and returns its response.
// A Response with a non-empty Error is returned as an error.
func Call(ctx context.Context, path string, req Request) (*Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("control: connecting to %s: %w", path, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else if req.Command != CmdWaitSync || req.Timeout > 0 {
		wait := 10 * time.Second
		if req.Timeout > 0 {
			wait += time.Duration(req.Timeout * float64(time.Second))
		}
		_ = conn.SetDeadline(time.Now().Add(wait))
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("control: sending request: %w", err)
	}
	r := bufio.NewReaderSize(conn, 64*1024)
	line, err := readLine(r, maxResponse)
	if err != nil {
		return nil, fmt.Errorf("control: reading response: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("control: decoding response: %w", err)
	}
	if resp.Error != "" {
		return &resp, errors.New(resp.Error)
	}
	return &resp, nil
}

// WaitSync asks the daemon at path to wait until the clock is synchronized,
// for at most timeout (zero waits indefinitely), and reports whether it was.
// Unlike Call it tolerates a daemon that is still starting: while the socket
// is missing or nothing is listening on it, WaitSync keeps retrying until
// the deadline, so it can directly follow a service restart. If the daemon
// never appears, the last connection error is returned.
func WaitSync(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	var lastErr error
	for {
		req := Request{Command: CmdWaitSync}
		if timeout > 0 {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return false, lastErr
			}
			req.Timeout = remaining.Seconds()
		}
		resp, err := Call(ctx, path, req)
		if err == nil {
			return resp.Synced != nil && *resp.Synced, nil
		}
		if !daemonStarting(err) {
			return false, err
		}
		lastErr = err
		wait := connectRetry
		if timeout > 0 {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return false, err
			}
			if remaining < wait {
				wait = remaining
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// daemonStarting reports whether err is the kind of connection failure a
// daemon that has not finished starting would cause: no socket file yet, or
// a socket file nobody listens on.
func daemonStarting(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, err := r.ReadSlice('\n')
		out = append(out, chunk...)
		if len(out) > limit {
			return nil, fmt.Errorf("response exceeds %d bytes", limit)
		}
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			// A complete but unterminated response is still a response.
			// Comparing the error text worked only for a bare io.EOF; net
			// wraps read errors in *net.OpError on some paths, which turned
			// a good reply into "reading response: EOF".
			if len(out) > 0 && errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
	}
}
