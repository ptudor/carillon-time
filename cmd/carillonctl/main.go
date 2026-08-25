// Command carillonctl shows the state of a running carillon daemon over its
// control socket.
//
// Usage:
//
//	carillonctl [-socket path] [-json] tracking
//	carillonctl [-socket path] [-json] sources
//	carillonctl [-socket path] [-json] refclock
//	carillonctl [-socket path] [-json] serverstats
//	carillonctl [-socket path] waitsync [seconds]
//	carillonctl [-socket path] version
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"carillon/internal/config"
	"carillon/internal/control"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("carillonctl", flag.ContinueOnError)
	sock := fs.String("socket", config.Default().Daemon.Control, "control socket path")
	asJSON := fs.Bool("json", false, "print the raw JSON response")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: carillonctl [-socket path] [-json] tracking|sources|refclock|serverstats|version\n       carillonctl [-socket path] waitsync [seconds]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fs.Usage()
		return 2
	}
	req := control.Request{Command: fs.Arg(0)}
	if req.Command == control.CmdWaitSync && fs.NArg() == 2 {
		secs, err := strconv.ParseFloat(fs.Arg(1), 64)
		if err != nil || secs < 0 {
			fmt.Fprintf(os.Stderr, "carillonctl: bad timeout %q\n", fs.Arg(1))
			return 2
		}
		req.Timeout = secs
	} else if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	var resp *control.Response
	var err error
	if req.Command == control.CmdWaitSync {
		// waitsync usually follows a service restart, so it waits for the
		// socket to appear rather than failing on the first dial.
		synced, werr := control.WaitSync(context.Background(), *sock, time.Duration(req.Timeout*float64(time.Second)))
		resp, err = &control.Response{Synced: &synced}, werr
	} else {
		resp, err = control.Call(context.Background(), *sock, req)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "carillonctl: %v\n", err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
		if resp.Synced != nil && !*resp.Synced {
			return 1
		}
		return 0
	}
	switch req.Command {
	case control.CmdVersion:
		fmt.Println(resp.Version)
	case control.CmdTracking:
		printTracking(resp.Tracking)
	case control.CmdSources:
		printSources(resp.Sources)
	case control.CmdRefclock:
		printRefclocks(resp.Refclocks)
	case control.CmdServerStats:
		printServerStats(resp.ServerStats)
	case control.CmdWaitSync:
		if resp.Synced == nil || !*resp.Synced {
			fmt.Println("not synchronized")
			return 1
		}
		fmt.Println("synchronized")
	}
	return 0
}

func printRefclocks(rs []control.Refclock) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "NAME\tTYPE\tDEVICE\tEDGE\tREACH\tPOLL\tWINDOW σ\tQUALIFIED\tLOCKED\tFIX/SATS\tLAG\tSEQ\tGAPS/GLITCHES/SPIKES")
	for _, r := range rs {
		fix := "-"
		if r.FixKnown {
			fix = fmt.Sprintf("%v/%d", r.FixValid, r.Satellites)
		}
		lag := "-"
		if r.LagSamples != 0 {
			lag = seconds(r.MeasuredLag, false)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%v\t%v\t%s\t%s\t%d\t%d/%d/%d\n",
			r.Name, r.Type, r.Device, r.Edge, control.ReachOctal(r.Reach), r.Poll,
			seconds(r.WindowJitter, false), r.Qualified, r.Locked, fix, lag,
			r.Sequence, r.Gaps, r.Glitches, r.Spikes)
	}
}

func printServerStats(s *control.ServerStats) {
	counts := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	row := func(label string, pick func(*control.CounterStats) uint64) {
		fmt.Fprintf(counts, "%s\t%d\t%d\t%d\n", label, pick(&s.CounterStats), pick(&s.IPv4), pick(&s.IPv6))
	}
	fmt.Fprint(counts, "\ttotal\tipv4\tipv6\n")
	row("Served", func(c *control.CounterStats) uint64 { return c.Served })
	row("  while unsynchronized", func(c *control.CounterStats) uint64 { return c.Unsynced })
	row("Dropped", (*control.CounterStats).Dropped)
	row("  outside the ACL", func(c *control.CounterStats) uint64 { return c.Denied })
	row("  martian address", func(c *control.CounterStats) uint64 { return c.Martian })
	row("  rate limited", func(c *control.CounterStats) uint64 { return c.RateLimited })
	row("  bad authentication", func(c *control.CounterStats) uint64 { return c.BadAuth })
	row("  bad version", func(c *control.CounterStats) uint64 { return c.BadVersion })
	row("  non-client mode", func(c *control.CounterStats) uint64 { return c.NonClient })
	row("  malformed", func(c *control.CounterStats) uint64 { return c.Malformed })
	row("  oversize", func(c *control.CounterStats) uint64 { return c.Oversize })
	row("Rate kiss replies sent", func(c *control.CounterStats) uint64 { return c.KoD })
	row("Missing kernel timestamps", func(c *control.CounterStats) uint64 { return c.NoKernelTS })
	row("Kernel receive drops", func(c *control.CounterStats) uint64 { return c.KernelDrops })
	fmt.Fprintf(counts, "Clients tracked\t%d\t%d\t%d\n", s.Clients, s.IPv4.Clients, s.IPv6.Clients)
	counts.Flush()

	// The wide free-text rows get their own writer so a long histogram does
	// not stretch the counter columns.
	detail := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer detail.Flush()
	fmt.Fprintf(detail, "Client versions\t%s\n", histogram(s.Versions, control.VersionOrder, "v"))
	fmt.Fprintf(detail, "Refused modes\t%s\n", histogram(s.Modes, control.ModeOrder, ""))
	if s.LastRequest != nil {
		fmt.Fprintf(detail, "Last request\t%s\n", s.LastRequest.UTC().Format(time.RFC3339Nano))
	}
	if s.LastServed != nil {
		fmt.Fprintf(detail, "Last served\t%s\n", s.LastServed.UTC().Format(time.RFC3339Nano))
	}
}

// histogram renders a counter map in the given order, skipping buckets that
// were never used so a quiet server shows a short line instead of a row of
// zeros. prefix decorates each key, so versions read as "v4" rather than "4".
func histogram(counts map[string]uint64, order []string, prefix string) string {
	parts := make([]string, 0, len(counts))
	for _, key := range order {
		if v := counts[key]; v != 0 {
			parts = append(parts, fmt.Sprintf("%s%s=%d", prefix, key, v))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func printTracking(t *control.Tracking) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "State\t%s\n", t.State)
	fmt.Fprintf(w, "Stratum\t%d\n", t.Stratum)
	fmt.Fprintf(w, "Reference ID\t%s\n", t.RefID)
	if t.SystemSource != "" {
		fmt.Fprintf(w, "System source\t%s\n", t.SystemSource)
	}
	if t.PreferLost {
		fmt.Fprintf(w, "Prefer\tLOST\n")
	}
	if !t.RefTime.IsZero() {
		fmt.Fprintf(w, "Reference time\t%s (%s ago)\n", t.RefTime.UTC().Format(time.RFC3339Nano), t.Now.Sub(t.RefTime).Round(time.Millisecond))
	}
	fmt.Fprintf(w, "System time\t%s\n", t.Now.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(w, "Last offset\t%s\n", seconds(t.Offset, true))
	fmt.Fprintf(w, "Pending slew\t%s\n", seconds(t.Pending, true))
	known := "measured"
	if !t.FreqKnown {
		known = "not yet known"
	}
	fmt.Fprintf(w, "Frequency\t%+.3f ppm (%s)\n", t.Frequency, known)
	fmt.Fprintf(w, "Jitter\t%s\n", seconds(t.Jitter, false))
	fmt.Fprintf(w, "Root delay\t%s\n", seconds(t.RootDelay, false))
	fmt.Fprintf(w, "Root dispersion\t%s\n", seconds(t.RootDisp, false))
	fmt.Fprintf(w, "Leap\t%s\n", t.Leap)
	fmt.Fprintf(w, "Leap source\t%s\n", t.LeapSource)
	if !t.LeapExpiry.IsZero() {
		fmt.Fprintf(w, "Leap file expires\t%s\n", t.LeapExpiry.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(w, "Updates / steps\t%d / %d\n", t.Updates, t.Steps)
	fmt.Fprintf(w, "Uptime\t%s\n", (time.Duration(t.Uptime * float64(time.Second))).Round(time.Second))
	fmt.Fprintf(w, "Version\t%s\n", t.Version)
}

func printSources(ss []control.Source) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "NAME\tSTATUS\tST\tREACH\tPOLL\tOFFSET\tDELAY\tJITTER\tDISTANCE\tADDRESS")
	for _, s := range ss {
		flags := ""
		if s.Prefer {
			flags += "*"
		}
		if s.NoSelect {
			flags += "-"
		}
		st := "-"
		if s.Status != "unreachable" {
			st = strconv.Itoa(int(s.Stratum))
		}
		addr := s.Resolved
		if addr == "" {
			addr = s.Address
		}
		if s.Denied {
			addr += " (denied)"
		}
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			flags, s.Name, s.Status, st, control.ReachOctal(s.Reach), s.Poll,
			seconds(s.Offset, true), seconds(s.Delay, false), seconds(s.Jitter, false), seconds(s.Distance, false), addr)
	}
}

// seconds renders a duration in seconds with a unit that keeps the
// significant digits visible.
func seconds(v float64, signed bool) string {
	sign := ""
	if signed && v >= 0 {
		sign = "+"
	}
	a := math.Abs(v)
	switch {
	case a == 0:
		return sign + "0"
	case a < 1e-3:
		return fmt.Sprintf("%s%.1f µs", sign, v*1e6)
	case a < 1:
		return fmt.Sprintf("%s%.3f ms", sign, v*1e3)
	default:
		return fmt.Sprintf("%s%.3f s", sign, v)
	}
}
