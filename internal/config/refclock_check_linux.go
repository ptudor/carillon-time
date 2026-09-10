//go:build linux

package config

import "fmt"

// checkRefclockPlatform enforces the one restriction Linux serial PPS really
// has: `pps_ldisc` timestamps DCD transitions and nothing else, so CTS is a
// FreeBSD-only pin.
//
// NMEA and PPS may share one tty. `pps_ldisc` is built with
// `n_tty_inherit_ops()` and its open handler chains to `n_tty_open()`, so
// N_PPS is N_TTY plus a `dcd_change` hook: reads keep delivering the sentence
// stream while carrier transitions are timestamped in the interrupt handler.
// That is what `ldattach(8) PPS` has always relied on, and carillon attaching
// the discipline itself is the same operation without the extra daemon.
//
// carillon used to refuse `pps = "dcd"` on the belief that N_PPS replaced
// normal tty input, which forced every Linux GPS host to run ldattach or wire
// a second port for no reason.
func checkRefclockPlatform(r *Refclock) error {
	if r.Type != "gps" || !r.HasPPS() {
		return nil
	}
	if r.PPS == "cts" {
		return fmt.Errorf("Linux serial PPS captures DCD only; set pps to \"dcd\", to a /dev/ppsN, or wire the pulse to DCD")
	}
	return nil
}
