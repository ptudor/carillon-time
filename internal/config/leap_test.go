package config

import (
	"strings"
	"testing"
)

func TestLeapPolicy(t *testing.T) {
	for _, c := range []struct {
		name, body, mode string
		required         bool
		want             string
	}{
		{"plain client", "[[server]]\naddress='pool.ntp.org'", "off", false, ""},
		{"manual default", "[daemon]\nleapfile='/tmp/leap-seconds.list'\n[[server]]\naddress='pool.ntp.org'", "manual", false, ""},
		{"peer default", "[daemon]\nkeys='/tmp/keys'\n[[server]]\naddress='192.0.2.1'\nkey=1\nleap_trust=true", "peers", false, ""},
		{"seed server", "[leap]\nacquire='nist'\n[[server]]\naddress='pool.ntp.org'\n[serve]\nlisten=['127.0.0.1:123']\nallow=['127.0.0.0/8']", "nist", true, ""},
		{"IERS seed server", "[leap]\nacquire='iers'\n[[server]]\naddress='pool.ntp.org'\n[serve]\nlisten=['127.0.0.1:123']\nallow=['127.0.0.0/8']", "iers", true, ""},
		{"IANA is not a publisher", "[leap]\nacquire='iana'\n[[server]]\naddress='pool.ntp.org'", "iana", false, "must be off, manual, nist, iers or peers"},
		{"IERS seed cannot also be manual", "[daemon]\nleapfile='/tmp/leap'\n[leap]\nacquire='iers'\n[[server]]\naddress='pool.ntp.org'", "iers", false, "requires manual"},
		{"GPS requires a table", "[[refclock]]\ntype='gps'\ndevice='/dev/ttyS0'", "off", true, "required table"},
		{"GPS cannot opt out", "[leap]\nacquire='nist'\nrequire_table=false\n[[refclock]]\ntype='gps'\ndevice='/dev/ttyS0'", "nist", false, "cannot be false"},
		{"server requires a table", "[[server]]\naddress='pool.ntp.org'\n[serve]\nallow=['127.0.0.0/8']", "off", true, "required table"},
		{"manual needs path", "[leap]\nacquire='manual'\n[[server]]\naddress='pool.ntp.org'", "manual", false, "requires daemon.leapfile"},
		{"ambiguous input", "[daemon]\nleapfile='/tmp/leap'\n[leap]\nacquire='nist'\n[[server]]\naddress='pool.ntp.org'", "nist", false, "requires manual"},
		{"peers need trust", "[leap]\nacquire='peers'\n[[server]]\naddress='pool.ntp.org'", "peers", false, "requires a leap-trusted"},
		{"trust needs CMAC", "[[server]]\naddress='192.0.2.1'\nleap_trust=true", "peers", false, "nonzero CMAC"},
		{"exports need listener", "[daemon]\nkeys='/tmp/keys'\n[[server]]\naddress='pool.ntp.org'\n[serve]\nleap_keys=[1]", "off", false, "enabled NTP listener"},
		{"exports need unique valid keys", "[daemon]\nkeys='/tmp/keys'\n[leap]\nacquire='nist'\n[[server]]\naddress='pool.ntp.org'\n[serve]\nallow=['127.0.0.0/8']\nleap_keys=[0,1,1]", "nist", true, "invalid or duplicate"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Parse([]byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LeapMode() != c.mode || cfg.LeapRequired() != c.required {
				t.Fatalf("defaults: mode=%s required=%t", cfg.LeapMode(), cfg.LeapRequired())
			}
			err = Validate(cfg)
			if c.want == "" && err != nil {
				t.Fatal(err)
			}
			if c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("wanted %q: %v", c.want, err)
			}
		})
	}
	body := "[daemon]\nkeys='/tmp/keys'\n"
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		body += "[[server]]\naddress='" + name + "'\nkey=1\nleap_trust=true\n"
	}
	cfg, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(cfg); err == nil || !strings.Contains(err.Error(), "four") {
		t.Fatalf("unbounded peer list: %v", err)
	}
}
