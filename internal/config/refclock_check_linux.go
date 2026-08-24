//go:build linux

package config

import "fmt"

func checkRefclockPlatform(r *Refclock) error {
	if r.Type != "gps" || !r.HasPPS() {
		return nil
	}
	if r.PPS == "dcd" || r.PPS == "cts" || r.PPS == r.Device {
		return fmt.Errorf("GPS serial data and Linux N_PPS cannot share one tty; set pps to a separate /dev/ppsN, PPS GPIO, or PPS-only tty")
	}
	return nil
}
