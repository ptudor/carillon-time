package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// maxResponse bounds a response line.
const maxResponse = 1 << 20

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
			if len(out) > 0 && err.Error() == "EOF" {
				return out, nil
			}
			return nil, err
		}
	}
}
