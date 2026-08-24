// Command carillon is an NTP-compatible time daemon. See DESIGN.md.
//
// Usage:
//
//	carillon [-config path] [-check] [-version]
//	carillon query [-timeout d] [-keys file -key id] host[:port]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"carillon/internal/buildinfo"
	"carillon/internal/clock"
	"carillon/internal/config"
	"carillon/internal/control"
	"carillon/internal/discipline"
	"carillon/internal/engine"
	"carillon/internal/monitor"
	"carillon/internal/ntp"
	"carillon/internal/ntp/auth"
	"carillon/internal/pps"
	"carillon/internal/refclock"
	ntpserver "carillon/internal/server"
	"carillon/internal/source"
)

// Exit codes: 1 runtime failure, 2 configuration or usage error.
const (
	exitRuntime = 1
	exitUsage   = 2
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "query" {
		os.Exit(runQuery(os.Args[2:]))
	}
	os.Exit(runDaemon(os.Args[1:]))
}

func runDaemon(args []string) int {
	fs := flag.NewFlagSet("carillon", flag.ContinueOnError)
	cfgPath := fs.String("config", config.DefaultPath(), "configuration file")
	check := fs.Bool("check", false, "validate the configuration and exit")
	version := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: carillon [-config path] [-check] [-version]\n       carillon query [-timeout d] [-keys file -key id] host[:port]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *version {
		fmt.Printf("carillon %s (built %s)\n", buildinfo.Version, buildTimeString())
		return 0
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return exitUsage
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "carillon: %v\n", err)
		return exitUsage
	}
	if *check {
		if err := config.Check(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "carillon: %v\n", err)
			return exitUsage
		}
		if _, err := loadKeys(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "carillon: %v\n", err)
			return exitUsage
		}
		listeners := 0
		if cfg.Serve.Enabled() {
			listeners = len(cfg.Serve.Listen)
		}
		monitors := 0
		if cfg.Monitor.Enabled() {
			monitors = 1
		}
		fmt.Printf("%s: configuration OK (%d upstreams, %d refclocks, %d NTP listeners, %d monitor listeners)\n", *cfgPath, len(cfg.Servers), len(cfg.Refclocks), listeners, monitors)
		return 0
	}

	log := newLogger(cfg.Daemon.LogLevel)
	log.Info("starting", "version", buildinfo.Version, "config", *cfgPath)

	if err := config.Check(cfg); err != nil {
		log.Error("configuration check failed", "error", err)
		return exitUsage
	}
	keys, err := loadKeys(cfg)
	if err != nil {
		log.Error("keys", "error", err)
		return exitUsage
	}

	clk, err := clock.New()
	if err != nil {
		if errors.Is(err, clock.ErrUnsupportedPlatform) {
			log.Error("this platform cannot discipline the system clock; only `carillon query` works here", "error", err)
		} else {
			log.Error("cannot open the system clock", "error", err)
		}
		return exitRuntime
	}
	log.Info("system clock", "precision_log2", clk.Precision())

	if err := probeNTPPort(log); err != nil {
		log.Error("refusing to start", "error", err)
		return exitRuntime
	}

	var specs []engine.SourceSpec
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		var key *auth.Key
		if s.Key != 0 {
			k := keys[s.Key]
			key = &k
		}
		src, err := source.NewNTP(source.NTPConfig{
			Name:    s.Name,
			Address: s.Address,
			Key:     key,
			IBurst:  s.IBurst,
			PollMin: int8(s.PollMin),
			PollMax: int8(s.PollMax),
		}, clk, log)
		if err != nil {
			log.Error("server", "name", s.Name, "error", err)
			return exitUsage
		}
		specs = append(specs, engine.SourceSpec{
			Source:  src,
			Options: discipline.Options{Prefer: s.Prefer, NoSelect: s.NoSelect, Numbering: true},
		})
	}
	for i := range cfg.Refclocks {
		r := &cfg.Refclocks[i]
		edge, err := pps.ParseEdge(r.Edge)
		if err != nil {
			log.Error("refclock", "name", r.Name, "error", err)
			return exitUsage
		}
		src, err := refclock.NewPPS(refclock.PPSConfig{
			Name: r.Name, Device: r.Device, Edge: edge, Offset: r.Offset,
			LockJitter: r.LockJitter, PollMin: int8(r.PollMin), PollMax: int8(r.PollMax),
		}, clk, log)
		if err != nil {
			log.Error("refclock", "name", r.Name, "error", err)
			return exitRuntime
		}
		specs = append(specs, engine.SourceSpec{
			Source: src,
			Options: discipline.Options{
				Prefer: r.Prefer, NoSelect: r.NoSelect, PPS: true,
			},
		})
	}

	eng, err := engine.New(engine.Config{
		Discipline: discipline.Config{
			Loop: discipline.LoopConfig{
				StepThreshold:  cfg.Step.Threshold,
				StepLimit:      cfg.Step.Limit,
				Panic:          cfg.Step.Panic,
				PanicAtStartup: cfg.Step.PanicAtStartup,
				MaxSlewPPM:     cfg.Discipline.MaxSlewPPM,
				Precision:      ntp.Log2Seconds(clk.Precision()),
				FreqMeasure:    900,
			},
			MinSurvivors:  cfg.Discipline.MinSurvivors,
			HoldoverMax:   cfg.Discipline.HoldoverMax,
			SettleUpdates: 3,
		},
		DriftFile: cfg.Daemon.DriftFile,
		Sources:   specs,
		Version:   buildinfo.Version,
	}, clk, log)
	if err != nil {
		log.Error("engine", "error", err)
		return exitRuntime
	}

	serverStats := &ntpserver.Stats{}
	var timeServer *ntpserver.Service
	if cfg.Serve.Enabled() {
		allow, deny, require := cfg.ServePrefixes()
		timeServer, err = ntpserver.Listen(ntpserver.ServiceConfig{
			Listen: cfg.ServeListenAddrs(),
			Handler: ntpserver.Config{
				Allow:        allow,
				Deny:         deny,
				RequireKey:   require,
				Keys:         keys,
				RateLimitPPS: cfg.Serve.RateLimitPPS,
				RateBurst:    cfg.Serve.RateBurst,
				KoD:          cfg.Serve.KoD,
				Status: func() ntpserver.SystemStatus {
					st := eng.Status()
					return ntpserver.SystemStatus{
						Synced:         st.State == discipline.StateSynced || st.State == discipline.StateHoldover,
						Leap:           st.Leap,
						Stratum:        st.Stratum,
						Precision:      st.Precision,
						RootDelay:      st.RootDelay,
						RootDispersion: st.RootDisp,
						ReferenceID:    st.RefID,
						ReferenceTime:  st.RefTime,
					}
				},
				Now:   clk.Now,
				Stats: serverStats,
			},
			Log: log,
		})
		if err != nil {
			log.Error("NTP server", "error", err)
			return exitRuntime
		}
		defer timeServer.Close()
		for _, addr := range timeServer.Addrs() {
			log.Info("NTP server listening", "address", addr)
		}
	}

	var monitorServer *monitor.Server
	if cfg.Monitor.Enabled() {
		monitorServer, err = monitor.Listen(monitor.Config{
			Listen:        cfg.MonitorListenAddr(),
			Allow:         cfg.MonitorPrefixes(),
			Metadata:      monitor.Metadata{ID: cfg.Monitor.ID, Name: cfg.Monitor.Name, Roles: cfg.Monitor.Roles},
			ServerEnabled: cfg.Serve.Enabled(),
			Status:        eng.Status,
			Stats:         serverStats,
			Now:           clk.Now,
			Log:           log,
		})
		if err != nil {
			log.Error("monitor", "error", err)
			return exitRuntime
		}
		defer monitorServer.Close()
		log.Info("monitor listening", "address", monitorServer.Addr())
	}

	ctl, err := control.Listen(cfg.Daemon.Control, eng, serverStats, buildinfo.Version, log)
	if err != nil {
		log.Error("control socket", "error", err)
		return exitRuntime
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	auxErr := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := ctl.Serve(ctx); err != nil {
			log.Error("control socket", "error", err)
			auxErr <- err
			stop()
		}
	}()
	if timeServer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := timeServer.Serve(ctx); err != nil {
				log.Error("NTP server", "error", err)
				auxErr <- err
				stop()
			}
		}()
	}
	if monitorServer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := monitorServer.Serve(ctx); err != nil {
				log.Error("monitor", "error", err)
				auxErr <- err
				stop()
			}
		}()
	}
	runErr := eng.Run(ctx)
	stop()
	wg.Wait()
	if runErr == nil {
		select {
		case runErr = <-auxErr:
		default:
		}
	}
	if runErr != nil {
		log.Error("daemon stopped", "error", runErr)
		return exitRuntime
	}
	log.Info("stopped")
	return 0
}

// loadKeys reads the keys file when configured and checks that every
// referenced key id exists.
func loadKeys(cfg *config.Config) (auth.Keys, error) {
	keys := auth.Keys{}
	if cfg.Daemon.Keys != "" {
		k, err := auth.LoadKeys(cfg.Daemon.Keys)
		if err != nil {
			return nil, err
		}
		keys = k
	}
	for _, s := range cfg.Servers {
		if s.Key == 0 {
			continue
		}
		if _, ok := keys[s.Key]; !ok {
			return nil, fmt.Errorf("server %q: key %d is not in %s", s.Name, s.Key, cfg.Daemon.Keys)
		}
	}
	for prefix, id := range cfg.Serve.RequireKey {
		if _, ok := keys[id]; !ok {
			return nil, fmt.Errorf("serve require_key %q: key %d is not in %s", prefix, id, cfg.Daemon.Keys)
		}
	}
	return keys, nil
}

// probeNTPPort refuses to run alongside another time daemon: two
// disciplines on one clock fight each other. Without the privilege to bind
// port 123 the probe is inconclusive and only logged.
func probeNTPPort(log *slog.Logger) error {
	pc, err := net.ListenPacket("udp", ":123")
	if err == nil {
		_ = pc.Close()
		return nil
	}
	if errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("UDP port 123 is in use: another time daemon (ntpd, chronyd, systemd-timesyncd) is running; stop it first")
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		log.Warn("cannot check whether another time daemon is running (no privilege to bind UDP 123)")
		return nil
	}
	log.Warn("cannot check whether another time daemon is running", "error", err)
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func buildTimeString() string {
	t := buildinfo.Time()
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

func runQuery(args []string) int {
	fs := flag.NewFlagSet("carillon query", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 2*time.Second, "reply timeout")
	keysPath := fs.String("keys", "", "keys file (ntp.keys syntax) for an authenticated query")
	keyID := fs.Uint("key", 0, "key id to authenticate with")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: carillon query [-timeout d] [-keys file -key id] host[:port]\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	var key *auth.Key
	if *keyID != 0 || *keysPath != "" {
		if *keyID == 0 || *keysPath == "" {
			fmt.Fprintln(os.Stderr, "carillon query: -keys and -key go together")
			return exitUsage
		}
		keys, err := auth.LoadKeys(*keysPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "carillon query: %v\n", err)
			return exitUsage
		}
		k, ok := keys[uint32(*keyID)]
		if !ok {
			fmt.Fprintf(os.Stderr, "carillon query: key %d not in %s\n", *keyID, *keysPath)
			return exitUsage
		}
		key = &k
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
	defer cancel()
	res, err := source.Query(ctx, fs.Arg(0), key, clock.ReadOnly(), *timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "carillon query: %v\n", err)
		return exitRuntime
	}
	p := &res.Packet
	fmt.Printf("server         %s\n", res.Server)
	fmt.Printf("stratum        %d   refid %s   leap %s   version %d   precision 2^%d\n",
		p.Stratum, p.ReferenceID, p.Leap, p.Version, p.Precision)
	fmt.Printf("offset         %+.6f s\n", res.Offset)
	fmt.Printf("delay          %.6f s\n", res.Delay)
	fmt.Printf("root delay     %.6f s   root dispersion %.6f s\n", p.RootDelay.Seconds(), p.RootDispersion.Seconds())
	fmt.Printf("reference time %s\n", p.ReferenceTime.Time(res.T3).UTC().Format(time.RFC3339Nano))
	fmt.Printf("t1 sent        %s\n", res.T1.UTC().Format(time.RFC3339Nano))
	fmt.Printf("t2 received    %s\n", res.T2.UTC().Format(time.RFC3339Nano))
	fmt.Printf("t3 replied     %s\n", res.T3.UTC().Format(time.RFC3339Nano))
	fmt.Printf("t4 got reply   %s\n", res.T4.UTC().Format(time.RFC3339Nano))
	fmt.Printf("kernel rx timestamp %v   authenticated %v\n", yesno(res.KernelTS), yesno(res.Authenticated))
	return 0
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
