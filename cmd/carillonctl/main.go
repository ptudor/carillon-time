// Command carillonctl shows the state of a running carillon daemon over its
// control socket.
//
// Usage:
//
//	carillonctl [-socket path] [-json] tracking
//	carillonctl [-socket path] [-json] sources
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
		fmt.Fprintf(fs.Output(), "usage: carillonctl [-socket path] [-json] tracking|sources|version\n       carillonctl [-socket path] waitsync [seconds]\n")
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

	resp, err := control.Call(context.Background(), *sock, req)
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
	case control.CmdWaitSync:
		if resp.Synced == nil || !*resp.Synced {
			fmt.Println("not synchronized")
			return 1
		}
		fmt.Println("synchronized")
	}
	return 0
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
