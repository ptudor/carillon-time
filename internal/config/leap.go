package config

func validateLeap(cfg *Config, needKeys *bool, fail func(string, ...any)) {
	mode := cfg.LeapMode()
	switch mode {
	case "off", "manual", "nist", "iers", "peers":
	default:
		fail("leap: acquire %q must be off, manual, nist, iers or peers", mode)
	}
	if (cfg.Serve.Enabled() || len(cfg.Refclocks) > 0) && !cfg.LeapRequired() {
		fail("leap: require_table cannot be false with NTP service or a refclock")
	}
	if cfg.LeapRequired() && mode == "off" {
		fail("leap: required table needs manual, nist, iers or peers acquisition")
	}
	if mode == "manual" && cfg.Daemon.LeapFile == "" {
		fail("leap: manual acquisition requires daemon.leapfile")
	}
	if mode != "manual" && cfg.Daemon.LeapFile != "" {
		fail("leap: daemon.leapfile requires manual acquisition")
	}
	if mode != "off" && cfg.Daemon.DriftFile == "" {
		fail("leap: acquisition requires daemon.drift_file to locate the state directory")
	}
	trusted := 0
	for _, s := range cfg.Servers {
		if !s.LeapTrust {
			continue
		}
		trusted++
		if s.Key == 0 {
			fail("server %q: leap_trust requires a nonzero CMAC key", s.Name)
		}
		*needKeys = true
	}
	if trusted > 4 {
		fail("leap: at most four leap-trusted servers are allowed")
	}
	if mode == "peers" && trusted == 0 {
		fail("leap: peers acquisition requires a leap-trusted server")
	}
	seen := map[uint32]bool{}
	for _, id := range cfg.Serve.LeapKeys {
		*needKeys = true
		if id == 0 || id > 65535 || seen[id] {
			fail("serve: leap_keys contains invalid or duplicate key %d", id)
		}
		seen[id] = true
	}
	if len(seen) > 0 && !cfg.Serve.Enabled() {
		fail("serve: leap_keys requires an enabled NTP listener")
	}
}
