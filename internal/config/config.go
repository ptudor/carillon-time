// Package config loads and validates the carillon TOML configuration.
//
// The decoder is strict: unknown keys are errors, so a typo cannot silently
// disable a setting. Validate is pure and reports every problem it finds in
// one error; Check performs the filesystem checks (paths, permissions) that
// `carillon -check` and daemon startup need.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"
)

// Poll exponent bounds (log2 seconds) accepted for upstream servers.
const (
	MinPoll = 2
	MaxPoll = 17
)

// Default field values for an upstream server.
const (
	DefaultPollMin = 6
	DefaultPollMax = 10
	DefaultPort    = 123
)

// Config is the whole configuration file.
type Config struct {
	Daemon     Daemon     `toml:"daemon"`
	Servers    []Server   `toml:"server"`
	Serve      Serve      `toml:"serve"`
	Discipline Discipline `toml:"discipline"`
	Step       Step       `toml:"step"`
}

// Daemon holds process-wide paths and logging.
type Daemon struct {
	// DriftFile stores the last known frequency correction (ppm) so a
	// restart does not start from zero.
	DriftFile string `toml:"drift_file"`

	// Control is the unix socket path carillonctl talks to.
	Control string `toml:"control"`

	// LeapFile is an optional IERS/NIST leap-seconds.list.
	LeapFile string `toml:"leapfile"`

	// Keys is the ntp.keys-format file holding AES-128-CMAC keys. Required
	// when any server has a key id.
	Keys string `toml:"keys"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `toml:"log_level"`
}

// Server is one upstream NTP server the daemon polls.
type Server struct {
	// Name identifies the server in status output; defaults to Address.
	Name string `toml:"name"`

	// Address is host, host:port, IPv4[:port], or [IPv6][:port]. The port
	// defaults to 123.
	Address string `toml:"address"`

	// Key is the id of the AES-128-CMAC key used to authenticate this
	// association; 0 means unauthenticated.
	Key uint32 `toml:"key"`

	// Prefer makes this the source whose offset is used unaltered whenever
	// it survives selection. At most one server may be preferred.
	Prefer bool `toml:"prefer"`

	// IBurst sends a burst of requests when the server becomes reachable,
	// for a fast first synchronization.
	IBurst bool `toml:"iburst"`

	// NoSelect keeps the server visible in status output but never uses
	// it for synchronization.
	NoSelect bool `toml:"noselect"`

	// PollMin and PollMax bound the poll exponent (log2 seconds). A zero
	// value means "not set" and is replaced by the default after decoding;
	// zero is outside the legal range so it cannot be mistaken for a real
	// value.
	PollMin int `toml:"poll_min"`
	PollMax int `toml:"poll_max"`
}

// Serve configures the NTP listener. It is disabled unless Allow contains at
// least one prefix; there is deliberately no implicit serve-everyone mode.
type Serve struct {
	// Listen is the numeric address and port for each UDP socket.
	Listen []string `toml:"listen"`

	// Deny is checked before Allow. A client not matched by Allow is denied.
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`

	// RequireKey maps client prefixes to mandatory AES-CMAC key ids.
	RequireKey map[string]uint32 `toml:"require_key"`

	// RateLimitPPS and RateBurst configure the per-client token bucket.
	RateLimitPPS float64 `toml:"rate_limit_pps"`
	RateBurst    float64 `toml:"rate_burst"`

	// KoD enables throttled RATE kiss replies for over-limit clients.
	KoD bool `toml:"kod"`
}

// Enabled reports whether the NTP server should be started.
func (s *Serve) Enabled() bool { return len(s.Allow) != 0 }

// Discipline tunes the selection and loop.
type Discipline struct {
	// MinSurvivors is the smallest number of sources the cluster algorithm
	// keeps.
	MinSurvivors int `toml:"min_survivors"`

	// HoldoverMax is how long (seconds) the daemon coasts on the held
	// frequency without any source before declaring itself unsynchronized.
	HoldoverMax float64 `toml:"holdover_max"`

	// MaxSlewPPM bounds the rate at which phase is corrected.
	MaxSlewPPM float64 `toml:"max_slew_ppm"`
}

// Step is the clock stepping policy (DESIGN.md §6.6).
type Step struct {
	// Threshold in seconds: offsets beyond it are stepped instead of slewed,
	// subject to Limit.
	Threshold float64 `toml:"threshold"`

	// Limit: -1 always allow steps, 0 never, N only within the first N loop
	// updates after start.
	Limit int `toml:"limit"`

	// Panic in seconds: refuse to correct offsets beyond this.
	Panic float64 `toml:"panic"`

	// PanicAtStartup allows a single correction beyond Panic for the very
	// first update, for hosts without a real-time clock.
	PanicAtStartup bool `toml:"panic_at_startup"`
}

// DefaultPath returns the configuration file path for this operating
// system.
func DefaultPath() string {
	if runtime.GOOS == "freebsd" {
		return "/usr/local/etc/carillon/carillon.toml"
	}
	return "/etc/carillon/carillon.toml"
}

// Default returns the built-in configuration for this operating system.
// Operating systems without a real clock backend (the development Mac) get
// the Linux paths so that parsing and validation behave the same everywhere.
func Default() *Config {
	drift := "/var/lib/carillon/drift"
	if runtime.GOOS == "freebsd" {
		drift = "/var/db/carillon/drift"
	}
	return &Config{
		Daemon: Daemon{
			DriftFile: drift,
			Control:   "/var/run/carillon/carillon.sock",
			LogLevel:  "info",
		},
		Serve: Serve{
			RateLimitPPS: 8,
			RateBurst:    16,
			KoD:          true,
		},
		Discipline: Discipline{
			MinSurvivors: 1,
			HoldoverMax:  3600,
			MaxSlewPPM:   500,
		},
		Step: Step{
			Threshold:      0.5,
			Limit:          3,
			Panic:          1000,
			PanicAtStartup: false,
		},
	}
}

// Load reads, parses and validates the file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes TOML on top of Default(). Scalar tables ([daemon],
// [discipline], [step]) keep their defaults for keys the document omits.
// [[server]] entries are whole values in TOML, so their defaults (name,
// poll range) are filled in per entry after decoding.
func Parse(data []byte) (*Config, error) {
	cfg := Default()
	dec := toml.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, renderTOMLError(err)
	}
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		if s.Name == "" {
			s.Name = s.Address
		}
		if s.PollMin == 0 {
			s.PollMin = DefaultPollMin
		}
		if s.PollMax == 0 {
			s.PollMax = DefaultPollMax
		}
	}
	return cfg, nil
}

// renderTOMLError turns go-toml's error types into messages that name the
// offending key and line.
func renderTOMLError(err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		msgs := make([]error, 0, len(strict.Errors))
		for i := range strict.Errors {
			e := &strict.Errors[i]
			row, _ := e.Position()
			msgs = append(msgs, fmt.Errorf("line %d: unknown key %q", row, strings.Join(e.Key(), ".")))
		}
		return errors.Join(msgs...)
	}
	var dec *toml.DecodeError
	if errors.As(err, &dec) {
		row, col := dec.Position()
		key := strings.Join(dec.Key(), ".")
		if key != "" {
			return fmt.Errorf("line %d col %d, key %q: %s", row, col, key, dec.Error())
		}
		return fmt.Errorf("line %d col %d: %s", row, col, dec.Error())
	}
	return err
}

// String names the server for messages.
func (s *Server) String() string {
	if s.Name != "" && s.Name != s.Address {
		return fmt.Sprintf("%s (%s)", s.Name, s.Address)
	}
	return s.Address
}

// HostPort splits Address into host and port. Accepted forms: host,
// host:port, IPv4, IPv4:port, [IPv6], [IPv6]:port, and a bare unbracketed
// IPv6 address (which then uses the default port).
func (s *Server) HostPort() (host string, port uint16, err error) {
	return splitHostPort(s.Address)
}

func splitHostPort(addr string) (string, uint16, error) {
	if addr == "" {
		return "", 0, errors.New("address is empty")
	}
	var host, portStr string
	hasPort := false
	switch {
	case strings.HasPrefix(addr, "["):
		end := strings.IndexByte(addr, ']')
		if end < 0 {
			return "", 0, fmt.Errorf("address %q: missing ']'", addr)
		}
		host = addr[1:end]
		rest := addr[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return "", 0, fmt.Errorf("address %q: unexpected %q after ']'", addr, rest)
			}
			portStr, hasPort = rest[1:], true
		}
		if _, perr := netip.ParseAddr(host); perr != nil {
			return "", 0, fmt.Errorf("address %q: bracketed host is not an IPv6 address", addr)
		}
	case strings.Count(addr, ":") > 1:
		// Only a bare IPv6 address has more than one colon.
		if _, perr := netip.ParseAddr(addr); perr != nil {
			return "", 0, fmt.Errorf("address %q: not a valid IPv6 address; bracket it to add a port", addr)
		}
		host = addr
	case strings.Contains(addr, ":"):
		i := strings.IndexByte(addr, ':')
		host, portStr, hasPort = addr[:i], addr[i+1:], true
	default:
		host = addr
	}
	if host == "" {
		return "", 0, fmt.Errorf("address %q: empty host", addr)
	}
	if strings.ContainsAny(host, " \t/") {
		return "", 0, fmt.Errorf("address %q: host contains invalid characters", addr)
	}
	port := uint16(DefaultPort)
	if hasPort {
		n, perr := strconv.ParseUint(portStr, 10, 16)
		if perr != nil || n == 0 {
			return "", 0, fmt.Errorf("address %q: invalid port %q", addr, portStr)
		}
		port = uint16(n)
	}
	return host, port, nil
}

var logLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

// Validate checks the configuration for internal consistency. It touches no
// files and reports every problem it finds in a single joined error.
func Validate(cfg *Config) error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if cfg.Daemon.DriftFile == "" {
		fail("daemon: drift_file must be set")
	}
	if cfg.Daemon.Control == "" {
		fail("daemon: control must be set")
	}
	if !logLevels[cfg.Daemon.LogLevel] {
		fail("daemon: log_level %q is not one of debug, info, warn, error", cfg.Daemon.LogLevel)
	}

	if len(cfg.Servers) == 0 {
		fail("no [[server]] configured: a daemon needs at least one upstream server (reference clocks arrive in a later milestone)")
	}
	names := make(map[string]bool, len(cfg.Servers))
	preferred := 0
	needKeys := false
	for i := range cfg.Servers {
		s := &cfg.Servers[i]
		label := fmt.Sprintf("server %q", s.Name)
		if s.Address == "" {
			fail("server #%d: address must be set", i+1)
		} else if _, _, err := s.HostPort(); err != nil {
			fail("%s: %v", label, err)
		}
		if s.Name == "" {
			fail("server #%d: name is empty", i+1)
		} else if names[s.Name] {
			fail("%s: duplicate name", label)
		}
		names[s.Name] = true
		if s.Key > 65535 {
			fail("%s: key %d out of range 1..65535", label, s.Key)
		}
		if s.Key != 0 {
			needKeys = true
		}
		if s.PollMin < MinPoll || s.PollMin > MaxPoll {
			fail("%s: poll_min %d out of range %d..%d", label, s.PollMin, MinPoll, MaxPoll)
		}
		if s.PollMax < MinPoll || s.PollMax > MaxPoll {
			fail("%s: poll_max %d out of range %d..%d", label, s.PollMax, MinPoll, MaxPoll)
		}
		if s.PollMin <= MaxPoll && s.PollMax >= MinPoll && s.PollMax < s.PollMin {
			fail("%s: poll_max %d is less than poll_min %d", label, s.PollMax, s.PollMin)
		}
		if s.Prefer {
			preferred++
			if s.NoSelect {
				fail("%s: prefer and noselect are mutually exclusive", label)
			}
		}
	}
	if preferred > 1 {
		fail("prefer is set on %d servers; at most one server may be preferred", preferred)
	}
	validateServe(&cfg.Serve, &needKeys, fail)
	if needKeys && cfg.Daemon.Keys == "" {
		fail("daemon: keys must be set when an upstream server or serve.require_key uses a key id")
	}

	if cfg.Discipline.MinSurvivors < 1 {
		fail("discipline: min_survivors %d must be at least 1", cfg.Discipline.MinSurvivors)
	}
	if !(cfg.Discipline.HoldoverMax > 0) {
		fail("discipline: holdover_max %v must be greater than 0", cfg.Discipline.HoldoverMax)
	}
	if !(cfg.Discipline.MaxSlewPPM > 0 && cfg.Discipline.MaxSlewPPM <= 500) {
		fail("discipline: max_slew_ppm %v must be in (0, 500]", cfg.Discipline.MaxSlewPPM)
	}

	if !(cfg.Step.Threshold > 0) {
		fail("step: threshold %v must be greater than 0", cfg.Step.Threshold)
	}
	if cfg.Step.Limit < -1 {
		fail("step: limit %d must be -1 (always), 0 (never), or a positive count", cfg.Step.Limit)
	}
	if !(cfg.Step.Panic > cfg.Step.Threshold) {
		fail("step: panic %v must be greater than threshold %v", cfg.Step.Panic, cfg.Step.Threshold)
	}

	return errors.Join(errs...)
}

func validateServe(s *Serve, needKeys *bool, fail func(string, ...any)) {
	if !s.Enabled() {
		if len(s.RequireKey) != 0 {
			fail("serve: require_key has entries but allow is empty, so the server is disabled")
		}
		return
	}
	if len(s.Listen) == 0 {
		fail("serve: listen must contain at least one address when allow is set")
	}
	seenListen := make(map[netip.AddrPort]bool, len(s.Listen))
	for _, raw := range s.Listen {
		a, err := netip.ParseAddrPort(raw)
		if err != nil || !a.Addr().IsValid() || a.Addr().Is4In6() || a.Port() == 0 {
			fail("serve: listen address %q must be a numeric IP:port with a nonzero port", raw)
			continue
		}
		if seenListen[a] {
			fail("serve: duplicate listen address %q", raw)
		}
		seenListen[a] = true
	}
	validatePrefixes := func(field string, values []string) {
		seen := make(map[netip.Prefix]bool, len(values))
		for _, raw := range values {
			p, err := netip.ParsePrefix(raw)
			if err != nil || p.Addr().Zone() != "" || p.Addr().Is4In6() {
				fail("serve: %s prefix %q is invalid", field, raw)
				continue
			}
			p = p.Masked()
			if seen[p] {
				fail("serve: duplicate %s prefix %q", field, raw)
			}
			seen[p] = true
		}
	}
	validatePrefixes("allow", s.Allow)
	validatePrefixes("deny", s.Deny)
	requirePrefixes := make([]string, 0, len(s.RequireKey))
	for raw := range s.RequireKey {
		requirePrefixes = append(requirePrefixes, raw)
	}
	slices.Sort(requirePrefixes)
	validatePrefixes("require_key", requirePrefixes)
	for _, raw := range requirePrefixes {
		id := s.RequireKey[raw]
		if id == 0 || id > 65535 {
			fail("serve: require_key prefix %q has key id %d outside 1..65535", raw, id)
		} else {
			*needKeys = true
		}
	}
	if !(s.RateLimitPPS > 0) {
		fail("serve: rate_limit_pps %v must be greater than zero", s.RateLimitPPS)
	}
	if !(s.RateBurst >= 1) {
		fail("serve: rate_burst %v must be at least 1", s.RateBurst)
	}
}

// ServePrefixes parses the already-validated ACL configuration for the
// server package.
func (c *Config) ServePrefixes() (allow, deny []netip.Prefix, require map[netip.Prefix]uint32) {
	for _, raw := range c.Serve.Allow {
		allow = append(allow, netip.MustParsePrefix(raw).Masked())
	}
	for _, raw := range c.Serve.Deny {
		deny = append(deny, netip.MustParsePrefix(raw).Masked())
	}
	require = make(map[netip.Prefix]uint32, len(c.Serve.RequireKey))
	for raw, id := range c.Serve.RequireKey {
		require[netip.MustParsePrefix(raw).Masked()] = id
	}
	return allow, deny, require
}

// ServeListenAddrs parses the already-validated listener addresses.
func (c *Config) ServeListenAddrs() []netip.AddrPort {
	out := make([]netip.AddrPort, 0, len(c.Serve.Listen))
	for _, raw := range c.Serve.Listen {
		out = append(out, netip.MustParseAddrPort(raw))
	}
	return out
}

// Check performs the filesystem checks that a valid configuration needs at
// runtime: the drift file (or its directory) is writable, the keys file
// exists and is private, the leap file is readable, and the control socket's
// directory exists. It reports every problem in one joined error.
func Check(cfg *Config) error {
	var errs []error

	if cfg.Daemon.DriftFile != "" {
		if err := checkWritableFileOrDir(cfg.Daemon.DriftFile); err != nil {
			errs = append(errs, fmt.Errorf("daemon: drift_file: %w", err))
		}
	}
	if cfg.Daemon.Control != "" {
		dir := filepath.Dir(cfg.Daemon.Control)
		if st, err := os.Stat(dir); err != nil {
			errs = append(errs, fmt.Errorf("daemon: control: directory %s: %w", dir, err))
		} else if !st.IsDir() {
			errs = append(errs, fmt.Errorf("daemon: control: %s is not a directory", dir))
		}
	}
	if cfg.Daemon.Keys != "" {
		if err := checkPrivateFile(cfg.Daemon.Keys); err != nil {
			errs = append(errs, fmt.Errorf("daemon: keys: %w", err))
		}
	}
	if cfg.Daemon.LeapFile != "" {
		f, err := os.Open(cfg.Daemon.LeapFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("daemon: leapfile: %w", err))
		} else {
			f.Close()
		}
	}
	return errors.Join(errs...)
}

// checkWritableFileOrDir accepts an existing writable regular file, or a
// missing file whose directory is writable.
func checkWritableFileOrDir(path string) error {
	st, err := os.Stat(path)
	switch {
	case err == nil:
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		if err := unix.Access(path, unix.W_OK); err != nil {
			return fmt.Errorf("%s is not writable: %w", path, err)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		dir := filepath.Dir(path)
		dst, derr := os.Stat(dir)
		if derr != nil {
			return fmt.Errorf("directory %s: %w", dir, derr)
		}
		if !dst.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		if err := unix.Access(dir, unix.W_OK); err != nil {
			return fmt.Errorf("directory %s is not writable: %w", dir, err)
		}
		return nil
	default:
		return err
	}
}

// checkPrivateFile requires a regular file readable by us and not readable
// by group or others.
func checkPrivateFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; it must not be group- or world-accessible (chmod 0600)", path, perm)
	}
	if err := unix.Access(path, unix.R_OK); err != nil {
		return fmt.Errorf("%s is not readable: %w", path, err)
	}
	return nil
}
