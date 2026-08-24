package config

import (
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const coloExample = `
[daemon]
drift_file = "/var/db/carillon/drift"
control    = "/var/run/carillon/carillon.sock"
keys       = "/usr/local/etc/carillon/keys"

[[server]]
name     = "home"
address  = "10.9.0.1:123"
key      = 1
prefer   = true
iburst   = true
poll_min = 4
poll_max = 6

[[server]]
name    = "pool-a"
address = "0.pool.ntp.org"
[[server]]
name    = "pool-b"
address = "1.pool.ntp.org"
[[server]]
name    = "pool-c"
address = "2.pool.ntp.org"

[serve]
listen         = ["0.0.0.0:123", "[::]:123"]
allow          = ["0.0.0.0/0", "::/0"]
deny           = ["192.0.2.0/24"]
require_key    = { "203.0.113.7/32" = 1 }
rate_limit_pps = 4
rate_burst     = 8
kod            = false

[discipline]
min_survivors = 1
holdover_max  = 3600
max_slew_ppm  = 500

[step]
threshold = 0.5
limit     = 3
`

func TestParseFull(t *testing.T) {
	cfg, err := Parse([]byte(coloExample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if cfg.Daemon.Keys != "/usr/local/etc/carillon/keys" || cfg.Daemon.LogLevel != "info" {
		t.Fatalf("daemon: %+v", cfg.Daemon)
	}
	if len(cfg.Servers) != 4 {
		t.Fatalf("servers: %d", len(cfg.Servers))
	}
	home := cfg.Servers[0]
	if home.Name != "home" || home.Key != 1 || !home.Prefer || !home.IBurst || home.PollMin != 4 || home.PollMax != 6 {
		t.Fatalf("home: %+v", home)
	}
	if cfg.Servers[1].PollMin != DefaultPollMin || cfg.Servers[1].PollMax != DefaultPollMax {
		t.Fatalf("pool-a defaults: %+v", cfg.Servers[1])
	}
	if !cfg.Serve.Enabled() || len(cfg.Serve.Listen) != 2 || cfg.Serve.RateLimitPPS != 4 || cfg.Serve.RateBurst != 8 || cfg.Serve.KoD {
		t.Fatalf("serve: %+v", cfg.Serve)
	}
	allow, deny, require := cfg.ServePrefixes()
	if len(allow) != 2 || len(deny) != 1 || require[netip.MustParsePrefix("203.0.113.7/32")] != 1 {
		t.Fatalf("parsed ACL: allow=%v deny=%v require=%v", allow, deny, require)
	}
	if got := cfg.ServeListenAddrs(); len(got) != 2 || got[0] != netip.MustParseAddrPort("0.0.0.0:123") {
		t.Fatalf("listen addrs: %v", got)
	}
	// Scalar tables keep defaults the document omitted.
	if cfg.Step.Panic != 1000 || cfg.Step.PanicAtStartup {
		t.Fatalf("step defaults not preserved: %+v", cfg.Step)
	}
	if cfg.Discipline.HoldoverMax != 3600 {
		t.Fatalf("discipline: %+v", cfg.Discipline)
	}
}

func TestParsePPSRefclock(t *testing.T) {
	cfg, err := Parse([]byte(`
[[refclock]]
name = "pps0"
type = "pps"
device = "/dev/pps0"
offset = 0.000001
prefer = true
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Refclocks) != 1 {
		t.Fatalf("refclocks: %+v", cfg.Refclocks)
	}
	r := cfg.Refclocks[0]
	if r.Edge != "assert" || r.PollMin != 4 || r.PollMax != 7 || r.LockJitter != 200e-6 || !r.Prefer {
		t.Fatalf("defaults: %+v", r)
	}
}

func TestParseUnknownKey(t *testing.T) {
	_, err := Parse([]byte("[daemon]\nlog_levle = \"info\"\n[[server]]\naddress = \"a\"\n"))
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "log_levle") {
		t.Fatalf("error does not name the key: %v", err)
	}
	_, err = Parse([]byte("[serve]\nrate_burts = 4\n"))
	if err == nil || !strings.Contains(err.Error(), "rate_burts") {
		t.Fatalf("unknown [serve] key must be rejected: %v", err)
	}
}

func TestParseSyntaxError(t *testing.T) {
	_, err := Parse([]byte("[daemon\nlog_level = \"info\"\n"))
	if err == nil {
		t.Fatal("expected syntax error")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("error does not carry a line number: %v", err)
	}
}

func TestServerDefaults(t *testing.T) {
	cfg, err := Parse([]byte("[[server]]\naddress = \"time.invalid\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Servers[0]
	if s.Name != "time.invalid" {
		t.Fatalf("name should default to address, got %q", s.Name)
	}
	if s.PollMin != DefaultPollMin || s.PollMax != DefaultPollMax {
		t.Fatalf("poll defaults: %d..%d", s.PollMin, s.PollMax)
	}
	if s.Key != 0 || s.Prefer || s.IBurst || s.NoSelect {
		t.Fatalf("flags should default off: %+v", s)
	}
	if cfg.Serve.Enabled() || cfg.Serve.RateLimitPPS != 8 || cfg.Serve.RateBurst != 16 || !cfg.Serve.KoD {
		t.Fatalf("serve defaults: %+v", cfg.Serve)
	}
	if s.String() != "time.invalid" {
		t.Fatalf("String: %q", s.String())
	}
	s.Name = "home"
	if s.String() != "home (time.invalid)" {
		t.Fatalf("String with name: %q", s.String())
	}
}

func TestHostPort(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port uint16
		ok   bool
	}{
		{"host", "host", 123, true},
		{"host:123", "host", 123, true},
		{"host:1234", "host", 1234, true},
		{"192.0.2.1", "192.0.2.1", 123, true},
		{"192.0.2.1:5000", "192.0.2.1", 5000, true},
		{"[2001:db8::1]", "2001:db8::1", 123, true},
		{"[2001:db8::1]:1234", "2001:db8::1", 1234, true},
		{"2001:db8::1", "2001:db8::1", 123, true},
		{"", "", 0, false},
		{":123", "", 0, false},
		{"host:", "", 0, false},
		{"host:0", "", 0, false},
		{"host:99999", "", 0, false},
		{"host:abc", "", 0, false},
		{"[2001:db8::1", "", 0, false},
		{"[2001:db8::1]x", "", 0, false},
		{"[nothost]", "", 0, false},
		{"a:b:c", "", 0, false},
		{"ho st", "", 0, false},
	}
	for _, c := range cases {
		s := Server{Address: c.in}
		host, port, err := s.HostPort()
		if c.ok {
			if err != nil {
				t.Errorf("%q: unexpected error %v", c.in, err)
				continue
			}
			if host != c.host || port != c.port {
				t.Errorf("%q: got %s:%d want %s:%d", c.in, host, port, c.host, c.port)
			}
		} else if err == nil {
			t.Errorf("%q: expected error, got %s:%d", c.in, host, port)
		}
	}
}

func validConfig() *Config {
	cfg := Default()
	cfg.Servers = []Server{
		{Name: "a", Address: "a.invalid", PollMin: 6, PollMax: 10},
		{Name: "b", Address: "b.invalid", PollMin: 6, PollMax: 10},
	}
	return cfg
}

func enableServe(c *Config) {
	c.Serve.Listen = []string{"127.0.0.1:123"}
	c.Serve.Allow = []string{"127.0.0.0/8"}
}

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no sources", func(c *Config) { c.Servers = nil }, "at least one [[server]] or [[refclock]]"},
		{"duplicate name", func(c *Config) { c.Servers[1].Name = "a" }, "duplicate name"},
		{"empty address", func(c *Config) { c.Servers[0].Address = "" }, "address must be set"},
		{"bad address", func(c *Config) { c.Servers[0].Address = "[::1" }, "missing ']'"},
		{"key too big", func(c *Config) { c.Servers[0].Key = 70000 }, "key 70000 out of range"},
		{"poll_min low", func(c *Config) { c.Servers[0].PollMin = 1 }, "poll_min 1 out of range"},
		{"poll_max high", func(c *Config) { c.Servers[0].PollMax = 20 }, "poll_max 20 out of range"},
		{"poll inverted", func(c *Config) { c.Servers[0].PollMin, c.Servers[0].PollMax = 8, 6 }, "poll_max 6 is less than poll_min 8"},
		{"two prefer", func(c *Config) { c.Servers[0].Prefer, c.Servers[1].Prefer = true, true }, "at most one source may be preferred"},
		{"prefer+noselect", func(c *Config) { c.Servers[0].Prefer, c.Servers[0].NoSelect = true, true }, "mutually exclusive"},
		{"key without keys file", func(c *Config) { c.Servers[0].Key = 1 }, "keys must be set"},
		{"serve no listen", func(c *Config) { c.Serve.Allow = []string{"127.0.0.0/8"} }, "listen must contain"},
		{"serve bad listen", func(c *Config) { enableServe(c); c.Serve.Listen[0] = "localhost:123" }, "numeric IP:port"},
		{"serve zero port", func(c *Config) { enableServe(c); c.Serve.Listen[0] = "127.0.0.1:0" }, "nonzero port"},
		{"serve duplicate listen", func(c *Config) { enableServe(c); c.Serve.Listen = append(c.Serve.Listen, c.Serve.Listen[0]) }, "duplicate listen"},
		{"serve bad allow", func(c *Config) { enableServe(c); c.Serve.Allow[0] = "127.0.0.1" }, "allow prefix"},
		{"serve bad deny", func(c *Config) { enableServe(c); c.Serve.Deny = []string{"bad"} }, "deny prefix"},
		{"serve duplicate allow", func(c *Config) { enableServe(c); c.Serve.Allow = []string{"127.0.0.0/8", "127.0.0.1/8"} }, "duplicate allow"},
		{"serve require disabled", func(c *Config) { c.Serve.RequireKey = map[string]uint32{"127.0.0.1/32": 1} }, "server is disabled"},
		{"serve bad require prefix", func(c *Config) { enableServe(c); c.Serve.RequireKey = map[string]uint32{"bad": 1} }, "require_key prefix"},
		{"serve bad require id", func(c *Config) { enableServe(c); c.Serve.RequireKey = map[string]uint32{"127.0.0.1/32": 0} }, "outside 1..65535"},
		{"serve key without keys file", func(c *Config) { enableServe(c); c.Serve.RequireKey = map[string]uint32{"127.0.0.1/32": 1} }, "keys must be set"},
		{"serve zero rate", func(c *Config) { enableServe(c); c.Serve.RateLimitPPS = 0 }, "rate_limit_pps"},
		{"serve low burst", func(c *Config) { enableServe(c); c.Serve.RateBurst = 0.5 }, "rate_burst"},
		{"bad log level", func(c *Config) { c.Daemon.LogLevel = "verbose" }, "log_level"},
		{"empty drift", func(c *Config) { c.Daemon.DriftFile = "" }, "drift_file must be set"},
		{"empty control", func(c *Config) { c.Daemon.Control = "" }, "control must be set"},
		{"min_survivors", func(c *Config) { c.Discipline.MinSurvivors = 0 }, "min_survivors"},
		{"holdover_max", func(c *Config) { c.Discipline.HoldoverMax = 0 }, "holdover_max"},
		{"max_slew high", func(c *Config) { c.Discipline.MaxSlewPPM = 501 }, "max_slew_ppm"},
		{"max_slew zero", func(c *Config) { c.Discipline.MaxSlewPPM = 0 }, "max_slew_ppm"},
		{"step threshold", func(c *Config) { c.Step.Threshold = 0 }, "threshold"},
		{"step limit", func(c *Config) { c.Step.Limit = -2 }, "limit -2"},
		{"step panic", func(c *Config) { c.Step.Panic = 0.1 }, "panic 0.1 must be greater than threshold"},
		{"bad refclock type", func(c *Config) {
			c.Refclocks = []Refclock{{Name: "p", Type: "gps", Device: "/dev/null", Edge: "assert", LockJitter: 1e-6, PollMin: 4, PollMax: 7}}
		}, "not implemented"},
		{"bad refclock edge", func(c *Config) {
			c.Refclocks = []Refclock{{Name: "p", Type: "pps", Device: "/dev/null", Edge: "rising", LockJitter: 1e-6, PollMin: 4, PollMax: 7}}
		}, "not assert or clear"},
		{"bad refclock jitter", func(c *Config) {
			c.Refclocks = []Refclock{{Name: "p", Type: "pps", Device: "/dev/null", Edge: "assert", PollMin: 4, PollMax: 7}}
		}, "lock_jitter"},
		{"duplicate source name", func(c *Config) {
			c.Refclocks = []Refclock{{Name: "a", Type: "pps", Device: "/dev/null", Edge: "assert", LockJitter: 1e-6, PollMin: 4, PollMax: 7}}
		}, "duplicate name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := validConfig()
			if err := Validate(cfg); err != nil {
				t.Fatalf("baseline must validate: %v", err)
			}
			c.mutate(cfg)
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("expected error containing %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestValidateReportsAll(t *testing.T) {
	cfg := validConfig()
	cfg.Servers[0].PollMin = 1
	cfg.Daemon.LogLevel = "loud"
	cfg.Step.Threshold = 0
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"poll_min 1", "log_level", "threshold"} {
		if !strings.Contains(msg, want) {
			t.Errorf("joined error missing %q: %s", want, msg)
		}
	}
}

func TestStepLimitValues(t *testing.T) {
	for _, limit := range []int{-1, 0, 1, 3, 100} {
		cfg := validConfig()
		cfg.Step.Limit = limit
		if err := Validate(cfg); err != nil {
			t.Errorf("limit %d should be valid: %v", limit, err)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "carillon.toml")
	if err := os.WriteFile(path, []byte(coloExample), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Servers[0].Name != "home" {
		t.Fatalf("unexpected config: %+v", cfg.Servers[0])
	}
	if _, err := Load(filepath.Join(dir, "missing.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[[server]]\naddress = \"x\"\npoll_min = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "poll_min") {
		t.Fatalf("Load must validate: %v", err)
	}
}

func TestCheck(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig()
	cfg.Daemon.DriftFile = filepath.Join(dir, "state", "drift")
	cfg.Daemon.Control = filepath.Join(dir, "run", "carillon.sock")
	cfg.Daemon.Keys = filepath.Join(dir, "keys")
	cfg.Daemon.LeapFile = filepath.Join(dir, "leap-seconds.list")

	// Everything missing: four problems.
	err := Check(cfg)
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"drift_file", "control", "keys", "leapfile"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}

	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Daemon.Keys, []byte("1 AES128CMAC 00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Daemon.LeapFile, []byte("#\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Keys file too open.
	err = Check(cfg)
	if err == nil || !strings.Contains(err.Error(), "chmod 0600") {
		t.Fatalf("expected keys permission error, got %v", err)
	}
	if strings.Contains(err.Error(), "drift_file") || strings.Contains(err.Error(), "leapfile") {
		t.Fatalf("drift dir and leapfile should now pass: %v", err)
	}

	if err := os.Chmod(cfg.Daemon.Keys, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(cfg); err != nil {
		t.Fatalf("expected clean check, got %v", err)
	}

	// An existing drift file must be writable.
	if err := os.WriteFile(cfg.Daemon.DriftFile, []byte("0\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		if err := Check(cfg); err == nil || !strings.Contains(err.Error(), "not writable") {
			t.Fatalf("expected drift_file not writable, got %v", err)
		}
	}
	if err := os.Chmod(cfg.Daemon.DriftFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(cfg); err != nil {
		t.Fatalf("writable drift file: %v", err)
	}

	// Drift file whose directory does not exist.
	cfg.Daemon.DriftFile = filepath.Join(dir, "nope", "drift")
	if err := Check(cfg); err == nil || !strings.Contains(err.Error(), "drift_file") {
		t.Fatalf("expected drift directory error, got %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	if p := DefaultPath(); !strings.HasSuffix(p, "/carillon/carillon.toml") {
		t.Fatalf("unexpected default path %q", p)
	}
}
