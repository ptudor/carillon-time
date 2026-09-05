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
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sys/unix"

	ntpserver "carillon/internal/server"
)

// Poll exponent bounds (log2 seconds) accepted for upstream servers.
const (
	MinPoll = 2
	MaxPoll = 17
)

// Rate-limit table and socket buffer bounds for [serve].
const (
	// DefaultMaxClients is the server package's own fallback, referenced
	// rather than repeated so the two cannot drift apart.
	DefaultMaxClients = ntpserver.DefaultMaxClients
	MinMaxClients     = 256
	MaxMaxClients     = 8 << 20

	// MinRecvBuffer is low enough to be a deliberate choice and high enough
	// that it cannot be a units mistake; MaxRecvBuffer is what a kernel will
	// plausibly grant with a raised rmem_max.
	// Keep in step with server.minRecvBuffer, the floor the listener's
	// halving retry stops at; this package imports server, so the constant
	// lives in both places rather than being imported in a cycle.
	MinRecvBuffer = 64 << 10
	MaxRecvBuffer = 256 << 20
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
	Refclocks  []Refclock `toml:"refclock"`
	Serve      Serve      `toml:"serve"`
	Monitor    Monitor    `toml:"monitor"`
	Stats      Stats      `toml:"stats"`
	Discipline Discipline `toml:"discipline"`
	Step       Step       `toml:"step"`
}

// Refclock is one local PPS or GPS reference clock. A GPS receiver always
// creates a <name>/nmea source and, when PPS is enabled, a <name>/pps source.
type Refclock struct {
	Name   string `toml:"name"`
	Type   string `toml:"type"`
	Device string `toml:"device"`
	Edge   string `toml:"edge"`

	// Offset is added to each PPS offset for fixed cable/driver delay.
	Offset float64 `toml:"offset"`

	Prefer   bool `toml:"prefer"`
	NoSelect bool `toml:"noselect"`

	LockJitter float64 `toml:"lock_jitter"`
	PollMin    int     `toml:"poll_min"`
	PollMax    int     `toml:"poll_max"`

	// GPS-only serial, sentence and optional PPS settings.
	Baud       int      `toml:"baud"`
	PPS        string   `toml:"pps"`
	PPSEdge    string   `toml:"pps_edge"`
	PPSOffset  float64  `toml:"pps_offset"`
	NMEAOffset float64  `toml:"nmea_offset"`
	Sentences  []string `toml:"sentences"`
}

// HasPPS reports whether this refclock produces a PPS logical source.
func (r *Refclock) HasPPS() bool { return r.Type == "pps" || (r.Type == "gps" && r.PPS != "none") }

// SourceNames returns the logical source names produced by this refclock.
func (r *Refclock) SourceNames() []string {
	if r.Type != "gps" {
		return []string{r.Name}
	}
	names := []string{r.Name + "/nmea"}
	if r.HasPPS() {
		names = append(names, r.Name+"/pps")
	}
	return names
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

	// MaxClients bounds the per-listener rate-limit table. Entries expire
	// after a minute and the least recently used is evicted when the table
	// is full, so a flood of forged source addresses costs bounded memory
	// while the heavy hitters stay tracked.
	MaxClients int `toml:"max_clients"`

	// RecvBuffer is the SO_RCVBUF size in bytes for each listening socket.
	// Zero leaves the kernel default, which is a few hundred packets of
	// headroom and too little for a busy public server: an overflow is
	// otherwise visible only as clients that never got an answer.
	RecvBuffer int `toml:"recv_buffer"`

	// KoD enables throttled RATE kiss replies for over-limit clients.
	KoD bool `toml:"kod"`
}

// Enabled reports whether the NTP server should be started.
func (s *Serve) Enabled() bool { return len(s.Allow) != 0 }

// Monitor configures the read-only HTTP observability listener. It is
// disabled unless Listen is set. Allow always applies to the immediate TCP
// peer; proxy headers are deliberately outside this trust boundary.
type Monitor struct {
	Listen string   `toml:"listen"`
	Allow  []string `toml:"allow"`

	// ID, Name and Roles are operator-provided display metadata. They do not
	// affect daemon behavior or Prometheus labels.
	ID    string   `toml:"id"`
	Name  string   `toml:"name"`
	Roles []string `toml:"roles"`
}

// Enabled reports whether the monitoring HTTP server should be started.
func (m *Monitor) Enabled() bool { return m.Listen != "" }

// Stats configures optional daily UTC TSV statistics files.
type Stats struct {
	Dir string `toml:"dir"`
}

// Enabled reports whether daily statistics should be recorded.
func (s *Stats) Enabled() bool { return s.Dir != "" }

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

	// SettleUpdates is how many measurements for the system source must
	// arrive after a step before the clock is served as synchronized
	// (DESIGN.md §6.5). One is enough for the guards around it; raise it
	// on a path noisy enough that a single post-step sample is not
	// convincing.
	SettleUpdates int `toml:"settle_updates"`
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
			MaxClients:   DefaultMaxClients,
			KoD:          true,
		},
		Monitor: Monitor{
			Allow: []string{"127.0.0.0/8", "::1/128"},
		},
		Discipline: Discipline{
			MinSurvivors:  1,
			HoldoverMax:   3600,
			MaxSlewPPM:    500,
			SettleUpdates: 1,
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
	for i := range cfg.Refclocks {
		r := &cfg.Refclocks[i]
		if r.Name == "" {
			r.Name = r.Device
			if r.Name == "" {
				r.Name = r.Type
			}
		}
		if r.Type == "pps" && r.Edge == "" {
			r.Edge = "assert"
		}
		if r.Type == "gps" {
			if r.Baud == 0 {
				r.Baud = 9600
			}
			if r.PPS == "" {
				r.PPS = "none"
			}
			if r.PPSEdge == "" {
				r.PPSEdge = "assert"
			}
			if len(r.Sentences) == 0 {
				r.Sentences = []string{"RMC", "ZDA"}
			}
		}
		if r.LockJitter == 0 {
			r.LockJitter = 200e-6
		}
		if r.PollMin == 0 {
			r.PollMin = 4
		}
		if r.PollMax == 0 {
			r.PollMax = 7
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

	if len(cfg.Servers) == 0 && len(cfg.Refclocks) == 0 {
		fail("no time source configured: add at least one [[server]] or [[refclock]]")
	}
	names := make(map[string]bool, len(cfg.Servers)+len(cfg.Refclocks))
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
	for i := range cfg.Refclocks {
		r := &cfg.Refclocks[i]
		label := fmt.Sprintf("refclock %q", r.Name)
		if r.Name == "" {
			fail("refclock #%d: name is empty", i+1)
		}
		if r.Type != "pps" && r.Type != "gps" {
			fail("%s: type %q is not pps or gps", label, r.Type)
		}
		if r.Device == "" {
			fail("%s: device must be set", label)
		}
		for _, name := range r.SourceNames() {
			if names[name] {
				fail("%s: duplicate name %q for logical source", label, name)
			}
			names[name] = true
		}
		if r.Type == "pps" {
			if r.Edge != "assert" && r.Edge != "clear" {
				fail("%s: edge %q is not assert or clear", label, r.Edge)
			}
			if math.IsNaN(r.Offset) || math.IsInf(r.Offset, 0) {
				fail("%s: offset must be finite", label)
			}
		} else if r.Type == "gps" {
			validateGPSRefclock(r, label, fail)
		}
		if !(r.LockJitter > 0) || math.IsInf(r.LockJitter, 0) || math.IsNaN(r.LockJitter) {
			fail("%s: lock_jitter %v must be greater than zero", label, r.LockJitter)
		}
		if r.PollMin < MinPoll || r.PollMin > MaxPoll {
			fail("%s: poll_min %d out of range %d..%d", label, r.PollMin, MinPoll, MaxPoll)
		}
		if r.PollMax < MinPoll || r.PollMax > MaxPoll {
			fail("%s: poll_max %d out of range %d..%d", label, r.PollMax, MinPoll, MaxPoll)
		}
		if r.PollMin <= MaxPoll && r.PollMax >= MinPoll && r.PollMax < r.PollMin {
			fail("%s: poll_max %d is less than poll_min %d", label, r.PollMax, r.PollMin)
		}
		if r.Prefer {
			preferred++
			if r.NoSelect {
				fail("%s: prefer and noselect are mutually exclusive", label)
			}
		}
	}
	if preferred > 1 {
		fail("prefer is set on %d sources; at most one source may be preferred", preferred)
	}
	validateServe(&cfg.Serve, &needKeys, fail)
	validateMonitor(&cfg.Monitor, fail)
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
	if cfg.Discipline.SettleUpdates < 1 {
		fail("discipline: settle_updates %d must be at least 1", cfg.Discipline.SettleUpdates)
	}

	if !(cfg.Step.Threshold > 0) {
		fail("step: threshold %v must be greater than 0", cfg.Step.Threshold)
	}
	if cfg.Step.Limit < -1 {
		fail("step: limit %d must be -1 (always), 0 (never), or a positive count", cfg.Step.Limit)
	}
	if cfg.Stats.Enabled() && filepath.Clean(cfg.Stats.Dir) == "." {
		fail("stats: dir must name a directory")
	}
	if !(cfg.Step.Panic > cfg.Step.Threshold) {
		fail("step: panic %v must be greater than threshold %v", cfg.Step.Panic, cfg.Step.Threshold)
	}

	return errors.Join(errs...)
}

func validateGPSRefclock(r *Refclock, label string, fail func(string, ...any)) {
	if r.Edge != "" || r.Offset != 0 {
		fail("%s: edge and offset apply only to type pps; use pps_edge and pps_offset", label)
	}
	switch r.Baud {
	case 4800, 9600, 19200, 38400, 57600, 115200:
	default:
		fail("%s: baud %d is not one of 4800, 9600, 19200, 38400, 57600, 115200", label, r.Baud)
	}
	switch r.PPS {
	case "none", "dcd", "cts":
	default:
		if !filepath.IsAbs(r.PPS) {
			fail("%s: pps %q is not none, dcd, cts, or an absolute device path", label, r.PPS)
		}
	}
	if r.PPS != "none" && r.PPSEdge != "assert" && r.PPSEdge != "clear" {
		fail("%s: pps_edge %q is not assert or clear", label, r.PPSEdge)
	}
	if math.IsNaN(r.PPSOffset) || math.IsInf(r.PPSOffset, 0) {
		fail("%s: pps_offset must be finite", label)
	}
	if math.IsNaN(r.NMEAOffset) || math.IsInf(r.NMEAOffset, 0) {
		fail("%s: nmea_offset must be finite", label)
	}
	seen := make(map[string]bool, len(r.Sentences))
	for _, sentence := range r.Sentences {
		if sentence != "RMC" && sentence != "ZDA" {
			fail("%s: sentence %q is not RMC or ZDA", label, sentence)
		}
		if seen[sentence] {
			fail("%s: duplicate sentence %q", label, sentence)
		}
		seen[sentence] = true
	}
}

func validateMonitor(m *Monitor, fail func(string, ...any)) {
	if !m.Enabled() {
		if m.ID != "" || m.Name != "" || len(m.Roles) != 0 {
			fail("monitor: listen must be set when identity metadata is configured")
		}
		return
	}
	addr, err := netip.ParseAddrPort(m.Listen)
	if err != nil || !addr.Addr().IsValid() || addr.Addr().Is4In6() || addr.Port() == 0 {
		fail("monitor: listen address %q must be a numeric IP:port with a nonzero port", m.Listen)
	}
	if len(m.Allow) == 0 {
		fail("monitor: allow must contain at least one prefix")
	}
	seenAllow := make(map[netip.Prefix]bool, len(m.Allow))
	for _, raw := range m.Allow {
		p, err := netip.ParsePrefix(raw)
		if err != nil || p.Addr().Zone() != "" || p.Addr().Is4In6() {
			fail("monitor: allow prefix %q is invalid", raw)
			continue
		}
		p = p.Masked()
		if seenAllow[p] {
			fail("monitor: duplicate allow prefix %q", raw)
		}
		seenAllow[p] = true
	}
	if m.ID != "" && !validMonitorTag(m.ID) {
		fail("monitor: id %q must contain only letters, digits, '.', '_' or '-'", m.ID)
	}
	if len(m.Name) > 128 || strings.TrimSpace(m.Name) != m.Name {
		fail("monitor: name must be at most 128 bytes with no leading or trailing whitespace")
	}
	seenRole := make(map[string]bool, len(m.Roles))
	for _, role := range m.Roles {
		if !validMonitorTag(role) {
			fail("monitor: role %q must contain only letters, digits, '.', '_' or '-'", role)
			continue
		}
		if seenRole[role] {
			fail("monitor: duplicate role %q", role)
		}
		seenRole[role] = true
	}
}

func validMonitorTag(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
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
	if s.MaxClients < MinMaxClients || s.MaxClients > MaxMaxClients {
		fail("serve: max_clients %d out of range %d..%d", s.MaxClients, MinMaxClients, MaxMaxClients)
	}
	if s.RecvBuffer != 0 && (s.RecvBuffer < MinRecvBuffer || s.RecvBuffer > MaxRecvBuffer) {
		fail("serve: recv_buffer %d must be 0 (kernel default) or between %d and %d bytes",
			s.RecvBuffer, MinRecvBuffer, MaxRecvBuffer)
	}
}

// privatePrefixes are the address ranges a server can serve without being
// reachable from the public internet.
var privatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

// Widest prefix an operator can plausibly enumerate. A site is delegated a
// /16 of IPv4 or a /32 of IPv6 at the outside; anything broader than that is
// a slice of the address space rather than an allocation, and the hosts in it
// are not ones the operator knows.
const (
	siteBitsV4 = 16
	siteBitsV6 = 32
)

// PublicAllowPrefixes returns the entries of [serve] allow that open this
// server to hosts the operator cannot enumerate, so startup can say out loud
// that it is answering the internet. It reports the configured strings, not
// the masked forms, so the message quotes what the operator wrote.
func (c *Config) PublicAllowPrefixes() []string {
	var public []string
	for _, raw := range c.Serve.Allow {
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		if isInternetFacingPrefix(p.Masked()) {
			public = append(public, raw)
		}
	}
	return public
}

// isInternetFacingPrefix reports whether p reaches outside private address
// space and is broader than any one site's allocation. p must already be
// masked.
//
// Both halves matter. A globally routable /64 is a home or office delegation
// whose occupants the operator knows, and warning about it would be crying
// wolf; 2000::/3 is the whole global unicast range and is exactly what an NTP
// pool member writes.
func isInternetFacingPrefix(p netip.Prefix) bool {
	site := siteBitsV6
	if p.Addr().Is4() {
		site = siteBitsV4
	}
	if p.Bits() >= site {
		return false
	}
	for _, private := range privatePrefixes {
		// p is entirely inside private when it starts inside it and is no
		// broader than it.
		if p.Bits() >= private.Bits() && private.Contains(p.Addr()) {
			return false
		}
	}
	return true
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

// MonitorListenAddr parses the already-validated HTTP listener address.
func (c *Config) MonitorListenAddr() netip.AddrPort {
	return netip.MustParseAddrPort(c.Monitor.Listen)
}

// MonitorPrefixes parses the already-validated monitoring request ACL.
func (c *Config) MonitorPrefixes() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(c.Monitor.Allow))
	for _, raw := range c.Monitor.Allow {
		out = append(out, netip.MustParsePrefix(raw).Masked())
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
	if cfg.Stats.Enabled() {
		st, err := os.Stat(cfg.Stats.Dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("stats: dir %s: %w", cfg.Stats.Dir, err))
		} else if !st.IsDir() {
			errs = append(errs, fmt.Errorf("stats: dir %s is not a directory", cfg.Stats.Dir))
		} else if err := unix.Access(cfg.Stats.Dir, unix.W_OK); err != nil {
			errs = append(errs, fmt.Errorf("stats: dir %s is not writable: %w", cfg.Stats.Dir, err))
		}
	}
	for i := range cfg.Refclocks {
		r := &cfg.Refclocks[i]
		if err := checkRefclockDevice(r); err != nil {
			errs = append(errs, fmt.Errorf("refclock %q: %w", r.Name, err))
		}
	}
	return errors.Join(errs...)
}

func checkRefclockDevice(r *Refclock) error {
	if err := checkCharacterDevice(r.Device); err != nil {
		return err
	}
	if r.Type == "gps" && filepath.IsAbs(r.PPS) && r.PPS != r.Device {
		if err := checkCharacterDevice(r.PPS); err != nil {
			return fmt.Errorf("PPS %w", err)
		}
	}
	return checkRefclockPlatform(r)
}

func checkCharacterDevice(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("device %s: %w", path, err)
	}
	if st.Mode()&(os.ModeDevice|os.ModeCharDevice) != os.ModeDevice|os.ModeCharDevice {
		return fmt.Errorf("device %s is not a character device", path)
	}
	if err := unix.Access(path, unix.R_OK|unix.W_OK); err != nil {
		return fmt.Errorf("device %s is not readable and writable: %w", path, err)
	}
	return nil
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
