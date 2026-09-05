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
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"carillon/internal/buildinfo"
	"carillon/internal/clock"
	"carillon/internal/config"
	"carillon/internal/control"
	"carillon/internal/discipline"
	"carillon/internal/engine"
	"carillon/internal/leap"
	"carillon/internal/monitor"
	"carillon/internal/ntp"
	"carillon/internal/ntp/auth"
	"carillon/internal/pps"
	"carillon/internal/refclock"
	ntpserver "carillon/internal/server"
	"carillon/internal/source"
	"carillon/internal/stats"
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
		leapTable, err := loadLeapTable(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "carillon: %v\n", err)
			return exitUsage
		}
		for _, warning := range configurationWarnings(cfg, leapTable, time.Now()) {
			fmt.Fprintf(os.Stderr, "carillon: warning: %s\n", warning.message)
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
	leapTable, err := loadLeapTable(cfg)
	if err != nil {
		log.Error("leap file", "error", err)
		return exitUsage
	}
	reportConfigurationWarnings(log, cfg, leapTable, time.Now())

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

	// The measurement epoch is shared between the engine and every source:
	// the engine bumps it whenever a step or a leap makes work in flight
	// wrong, and each source stamps the value it read when it began a
	// sample. It is created here because the sources are built first.
	generation := new(atomic.Uint64)

	var specs []engine.SourceSpec
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		var key *auth.Key
		if s.Key != 0 {
			k := keys[s.Key]
			key = &k
		}
		host, port, err := s.HostPort()
		if err != nil {
			log.Error("server", "name", s.Name, "error", err)
			return exitUsage
		}
		src, err := source.NewNTP(source.NTPConfig{
			Name:       s.Name,
			Address:    s.Address,
			Host:       host,
			Port:       port,
			Key:        key,
			IBurst:     s.IBurst,
			PollMin:    int8(s.PollMin),
			PollMax:    int8(s.PollMax),
			Generation: generation.Load,
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
		if r.Type == "pps" {
			edge, err := pps.ParseEdge(r.Edge)
			if err != nil {
				log.Error("refclock", "name", r.Name, "error", err)
				return exitUsage
			}
			src, err := refclock.NewPPS(refclock.PPSConfig{
				Name: r.Name, Device: r.Device, Edge: edge, Offset: r.Offset,
				LockJitter: r.LockJitter, PollMin: int8(r.PollMin), PollMax: int8(r.PollMax),
				MaxSlewPPM: cfg.Discipline.MaxSlewPPM, Generation: generation.Load,
			}, clk, log)
			if err != nil {
				log.Error("refclock", "name", r.Name, "error", err)
				return exitRuntime
			}
			specs = append(specs, engine.SourceSpec{
				Source:  src,
				Options: discipline.Options{Prefer: r.Prefer, NoSelect: r.NoSelect, PPS: true},
			})
			continue
		}

		var pulse *refclock.PulseTracker
		if r.HasPPS() {
			pulse = &refclock.PulseTracker{}
		}
		nmea, err := refclock.NewNMEA(refclock.NMEAConfig{
			Name: r.Name + "/nmea", Device: r.Device, Baud: r.Baud,
			Offset: r.NMEAOffset, Sentences: r.Sentences, BuildTime: buildinfo.Time(), Pulse: pulse,
			Generation: generation.Load,
		}, clk, log)
		if err != nil {
			log.Error("refclock", "name", r.Name, "error", err)
			return exitRuntime
		}
		specs = append(specs, engine.SourceSpec{
			Source: nmea,
			Options: discipline.Options{
				Prefer: r.Prefer && !r.HasPPS(), NoSelect: r.NoSelect, Numbering: true,
			},
		})
		if !r.HasPPS() {
			continue
		}
		edge, err := pps.ParseEdge(r.PPSEdge)
		if err != nil {
			_ = nmea.Close()
			log.Error("refclock", "name", r.Name, "error", err)
			return exitUsage
		}
		ppsDevice := r.PPS
		if r.PPS == "dcd" || r.PPS == "cts" {
			ppsDevice = r.Device
		}
		pulseSource, err := refclock.NewPPS(refclock.PPSConfig{
			Name: r.Name + "/pps", Type: "gps-pps", Device: ppsDevice,
			Edge: edge, Offset: r.PPSOffset, LockJitter: r.LockJitter,
			PollMin: int8(r.PollMin), PollMax: int8(r.PollMax), OnPulse: pulse.Observe,
			MaxSlewPPM: cfg.Discipline.MaxSlewPPM, Generation: generation.Load,
		}, clk, log)
		if err != nil {
			_ = nmea.Close()
			log.Error("refclock", "name", r.Name, "error", err)
			return exitRuntime
		}
		specs = append(specs, engine.SourceSpec{
			Source:  pulseSource,
			Options: discipline.Options{Prefer: r.Prefer, NoSelect: r.NoSelect, PPS: true},
		})
	}

	// The listener counters are created before the recorder so the daily
	// statistics can include server traffic, and before the listener itself
	// so a bind failure cannot leave the recorder pointing at nothing.
	serverStats := &ntpserver.Stats{}

	var statsRecorder *stats.Recorder
	var observe func(*engine.Status)
	if cfg.Stats.Enabled() {
		scfg := stats.Config{Dir: cfg.Stats.Dir, KeepDays: cfg.Stats.KeepDays, Now: clk.Now, Log: log}
		if cfg.Serve.Enabled() {
			scfg.Server = serverStats.Snapshot
		}
		statsRecorder = stats.New(scfg)
		observe = statsRecorder.Record
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
			SettleUpdates: cfg.Discipline.SettleUpdates,
			LocalRefIDs:   localRefIDs(log),
		},
		DriftFile:         cfg.Daemon.DriftFile,
		DriftStableWindow: time.Duration(cfg.Daemon.DriftStableSeconds * float64(time.Second)),
		DriftStableSpread: cfg.Daemon.DriftStableSpreadPPM,
		Sources:           specs,
		LeapTable:         leapTable,
		Version:           buildinfo.Version,
		Observe:           observe,
		Generation:        generation,
	}, clk, log)
	if err != nil {
		log.Error("engine", "error", err)
		return exitRuntime
	}

	var timeServer *ntpserver.Service
	if cfg.Serve.Enabled() {
		allow, deny, require := cfg.ServePrefixes()
		timeServer, err = ntpserver.Listen(ntpserver.ServiceConfig{
			Listen: cfg.ServeListenAddrs(),
			Handler: ntpserver.Config{
				Allow:             allow,
				Deny:              deny,
				RequireKey:        require,
				Keys:              keys,
				RateLimitPPS:      cfg.Serve.RateLimitPPS,
				RateBurst:         cfg.Serve.RateBurst,
				RateLimitV6Prefix: cfg.Serve.RateLimitV6Prefix,
				MaxClients:        cfg.Serve.MaxClients,
				KoD:               cfg.Serve.KoD,
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
			RecvBuffer: cfg.Serve.RecvBuffer,
			Log:        log,
		})
		if err != nil {
			log.Error("NTP server", "error", err)
			return exitRuntime
		}
		defer timeServer.Close()
		buffers := timeServer.ReceiveBuffers()
		for i, addr := range timeServer.Addrs() {
			log.Info("NTP server listening", "address", addr, "recv_buffer", buffers[i])
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
	// Listen holds an exclusive lock on the control path for as long as the
	// daemon runs; release it on every exit, including the ones that never
	// reach Serve.
	defer ctl.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// SIGHUP is deliberately not a reload (DESIGN.md D11), but Go's default
	// action for an unhandled signal is to terminate the process at once:
	// no drift file, no base frequency restored to the kernel, sockets
	// closed by the OS. An operator sending HUP to ask for a reload, or a
	// terminal hangup on an interactively started instance, would get that.
	// Ignore it and say so the first time.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		if _, ok := <-hup; ok {
			log.Warn("SIGHUP ignored: carillon has no reload; restart it to pick up a changed configuration")
		}
		for range hup {
		}
	}()
	statsCtx, stopStats := context.WithCancel(context.Background())
	defer stopStats()
	auxErr := make(chan error, 3)
	aux := newAuxiliaries()
	if statsRecorder != nil {
		log.Info("statistics enabled", "directory", cfg.Stats.Dir)
		aux.start("statistics", func() { statsRecorder.Run(statsCtx) })
	}
	aux.start("control socket", func() {
		if err := ctl.Serve(ctx); err != nil {
			log.Error("control socket", "error", err)
			auxErr <- err
			stop()
		}
	})
	if timeServer != nil {
		aux.start("NTP server", func() {
			if err := timeServer.Serve(ctx); err != nil {
				log.Error("NTP server", "error", err)
				auxErr <- err
				stop()
			}
		})
	}
	if monitorServer != nil {
		aux.start("monitor", func() {
			if err := monitorServer.Serve(ctx); err != nil {
				log.Error("monitor", "error", err)
				auxErr <- err
				stop()
			}
		})
	}
	runErr := eng.Run(ctx)
	stopStats()
	stop()
	// The engine has already written the drift file and restored the base
	// frequency by the time Run returns, so the deadline below only bounds
	// how long a stuck auxiliary can delay the exit. Without it a client
	// that asked for `waitsync 0` and stopped reading held the control
	// server's handler open and `service carillon stop` hung until the init
	// system's own timeout killed the process.
	if stuck := aux.wait(shutdownDeadline); len(stuck) > 0 {
		log.Error("components did not stop before the shutdown deadline; exiting anyway",
			"components", strings.Join(stuck, ", "), "deadline", shutdownDeadline)
	}
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

// shutdownDeadline bounds the wait for the auxiliary goroutines (DESIGN.md
// §12). Exiting a few seconds late is better than not exiting at all.
const shutdownDeadline = 5 * time.Second

// auxiliaries tracks the long-running goroutines beside the engine so that
// one which fails to stop can be named rather than merely waited on.
type auxiliaries struct {
	wg      sync.WaitGroup
	mu      sync.Mutex
	running map[string]bool
}

func newAuxiliaries() *auxiliaries {
	return &auxiliaries{running: make(map[string]bool)}
}

func (a *auxiliaries) start(name string, f func()) {
	a.mu.Lock()
	a.running[name] = true
	a.mu.Unlock()
	a.wg.Add(1)
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.running, name)
			a.mu.Unlock()
			a.wg.Done()
		}()
		f()
	}()
}

// wait blocks until every auxiliary has stopped or the deadline passes,
// returning the names of those still running.
func (a *auxiliaries) wait(deadline time.Duration) []string {
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	t := time.NewTimer(deadline)
	defer t.Stop()
	select {
	case <-done:
		return nil
	case <-t.C:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.running))
	for n := range a.running {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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

func loadLeapTable(cfg *config.Config) (*leap.Table, error) {
	if cfg.Daemon.LeapFile == "" {
		return nil, nil
	}
	return leap.Load(cfg.Daemon.LeapFile)
}

type configurationWarning struct {
	message string
	error   bool
}

func configurationWarnings(cfg *config.Config, table *leap.Table, now time.Time) []configurationWarning {
	warnings := leapWarnings(cfg, table, now)
	return append(warnings, serveWarnings(cfg)...)
}

func leapWarnings(cfg *config.Config, table *leap.Table, now time.Time) []configurationWarning {
	var warnings []configurationWarning
	if table == nil {
		for i := range cfg.Refclocks {
			if cfg.Refclocks[i].Type == "gps" {
				warnings = append(warnings, configurationWarning{message: "GPS refclock has no leapfile; a stratum-1 server cannot announce leap seconds from NMEA alone"})
				break
			}
		}
		return warnings
	}
	remaining := table.Expiry.Sub(now.UTC())
	switch {
	case remaining <= 0:
		warnings = append(warnings, configurationWarning{
			message: fmt.Sprintf("leapfile expired at %s", table.Expiry.Format(time.RFC3339)), error: true,
		})
	case remaining <= 30*24*time.Hour:
		warnings = append(warnings, configurationWarning{
			message: fmt.Sprintf("leapfile expires in %s at %s", remaining.Round(time.Hour), table.Expiry.Format(time.RFC3339)),
		})
	}
	return warnings
}

// publicRateLimitPPS is the highest sustained per-client rate that still
// looks like a rate limit on a public server. chrony's default works out to
// one packet every eight seconds and ntpd's to the same; a real client polls
// once every 64 seconds or slower, so anything near one packet per second is
// a limit in name only.
const publicRateLimitPPS = 1

// serveWarnings says out loud what an ACL that reaches the public internet
// means, and flags the settings that were chosen for a LAN.
func serveWarnings(cfg *config.Config) []configurationWarning {
	if !cfg.Serve.Enabled() {
		return nil
	}
	public := cfg.PublicAllowPrefixes()
	if len(public) == 0 {
		return nil
	}
	warnings := []configurationWarning{{
		message: fmt.Sprintf("serving NTP to the public internet: allow includes %s", strings.Join(public, ", ")),
	}}
	if cfg.Serve.RateLimitPPS > publicRateLimitPPS {
		warnings = append(warnings, configurationWarning{
			message: fmt.Sprintf("rate_limit_pps %g lets one client draw %g replies a second from a public server; chrony and ntpd default to roughly one every eight seconds",
				cfg.Serve.RateLimitPPS, cfg.Serve.RateLimitPPS),
		})
	}
	if cfg.Serve.RecvBuffer == 0 {
		warnings = append(warnings, configurationWarning{
			message: "recv_buffer is unset, so each listening socket keeps the kernel default; a public server that outruns it drops requests with no other evidence",
		})
	} else if runtime.GOOS == "freebsd" {
		// FreeBSD refuses an oversized SO_RCVBUF with ENOBUFS rather than
		// clamping it the way Linux does. The listener halves the request
		// until the kernel accepts it, so the daemon still starts, but the
		// operator gets the buffer they asked for only after raising the
		// sysctl.
		warnings = append(warnings, configurationWarning{
			message: fmt.Sprintf("recv_buffer = %d: FreeBSD refuses a request above %s (default about 1.86 MB) instead of clamping it; raise the sysctl or the listener will settle for a smaller buffer",
				cfg.Serve.RecvBuffer, ntpserver.RecvBufferSysctl()),
		})
	}
	return warnings
}

func reportConfigurationWarnings(log *slog.Logger, cfg *config.Config, table *leap.Table, now time.Time) {
	for _, warning := range configurationWarnings(cfg, table, now) {
		if warning.error {
			log.Error(warning.message)
		} else {
			log.Warn(warning.message)
		}
	}
	if table != nil {
		if next, ok := table.Next(now.UTC()); ok {
			log.Info("next leap transition", "at", next.At.Format(time.RFC3339), "leap", next.Leap.String())
		}
	}
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

	host, port, err := config.ParseServerAddress(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "carillon query: %v\n", err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout+5*time.Second)
	defer cancel()
	res, err := source.Query(ctx, host, port, key, clock.ReadOnly(), *timeout)
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

// localRefIDs is the set of RFC 5905 §7.3 reference identifiers naming this
// host, one per local unicast address on every interface. Selection uses it
// to refuse a source that is this daemon or that is synchronized to it
// (RA6X-038).
//
// Every address is included, so a multihomed host is covered whichever one a
// peer reaches it on, and loopback is included so a configuration pointing at
// the local server is caught. The set is built once at startup, like the rest
// of the configuration; an address added later is not covered until a
// restart. Enumeration failing is not fatal — it only disables the check —
// because a daemon that cannot list its interfaces can still keep time.
func localRefIDs(log *slog.Logger) map[ntp.RefID]bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Warn("cannot enumerate local addresses; self-synchronization and timing loops will not be detected", "error", err)
		return nil
	}
	ids := make(map[ntp.RefID]bool, len(addrs))
	for _, a := range addrs {
		prefix, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(prefix.IP)
		if !ok {
			continue
		}
		ids[ntp.RefIDFromAddr(addr.Unmap())] = true
	}
	return ids
}
