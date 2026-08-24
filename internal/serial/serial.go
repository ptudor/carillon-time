// Package serial opens serial ports without letting a modem-control PPS
// signal hang up the descriptor. Platform files add the Linux PPS line
// discipline where needed.
package serial

import "errors"

// ErrTimeout means no serial data was readable before the requested timeout.
var ErrTimeout = errors.New("serial read timed out")

// Port is an open serial port whose descriptor is held for the lifetime of
// a reference clock.
type Port struct {
	fd   int
	path string
}

// FD returns the kernel descriptor.
func (p *Port) FD() int { return p.fd }

// Path returns the configured device path.
func (p *Port) Path() string { return p.path }
