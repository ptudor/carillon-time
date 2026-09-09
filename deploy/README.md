# carillon deployment quickstart

For GitHub release RPMs and DEBs, follow [Linux package setup](../docs/linux-packages.md).
Packages install under `/usr/bin` with a network-only systemd unit. The manual
recipes below use `/usr/local` and include example hardware access.

These recipes replace the host's existing NTP daemon. `carillon` refuses to
start while another process owns UDP/123, and two clock-discipline daemons
must never run together. Prepare the binary and configuration first so the
service outage is only the stop/start interval.

Build both target binaries from the repository root:

```sh
make linux
make freebsd
```

The outputs are under `dist/linux-amd64/` and `dist/freebsd-amd64/`. A host
that only consumes time can omit `[serve]`; serving remains off until
`serve.allow` contains a prefix. Shared AES-CMAC keys belong in the separate
keys file with mode `0600`, one line per key:

```text
1 AES128CMAC 0123456789abcdef0123456789abcdef
```

Use a freshly generated value in place of the example. Never put it in the
TOML file or version control.

## Linux with systemd

As root, install the account, state/config directories, binaries, unit, and
configuration:

```sh
groupadd --system carillon
useradd --system -g carillon -G dialout -d /nonexistent -s /usr/sbin/nologin carillon
install -d -o carillon -g carillon -m 0750 /var/lib/carillon
install -d -o root -g carillon -m 0750 /etc/carillon
install -d -o carillon -g carillon -m 0750 /run/carillon
install -o root -g root -m 0755 carillon /usr/local/sbin/carillon
install -o root -g root -m 0755 carillonctl /usr/local/bin/carillonctl
install -o root -g root -m 0644 carillon.service /etc/systemd/system/carillon.service
install -o root -g carillon -m 0640 carillon.toml /etc/carillon/carillon.toml
install -o carillon -g carillon -m 0600 keys /etc/carillon/keys
systemctl daemon-reload
usermod -a -G carillon YOUR_OPERATOR_ACCOUNT
```

The unit creates `/run/carillon` for the control socket and grants only
`CAP_SYS_TIME` and `CAP_NET_BIND_SERVICE`. Validate before interrupting the
old daemon:

```sh
/usr/local/sbin/carillon -check -config /etc/carillon/carillon.toml
```

For Linux PPS, install the line discipline and a udev rule before adding a
`[[refclock]]`. A configured tty makes carillon attach `N_PPS`; an existing
`/dev/ppsN` is opened directly. The rule gives the unprivileged daemon access
to either kind:

```sh
printf 'pps_ldisc\n' > /etc/modules-load.d/carillon.conf
modprobe pps_ldisc
printf 'SUBSYSTEM=="pps", GROUP="carillon", MODE="0660"\n' > /etc/udev/rules.d/60-carillon-pps.rules
udevadm control --reload-rules
udevadm trigger --subsystem-match=pps
```

Before enabling it, confirm that the sequence after `#` increases once per
second in `/sys/class/pps/pps0/assert` (or `clear` for the selected edge).

A Linux GPS receiver that carries NMEA on a tty cannot use serial `N_PPS` on
that same tty: the PPS line discipline replaces normal serial input. Keep
NMEA on the receiver tty and configure a separate kernel PPS device (commonly
`pps-gpio`) or a second PPS-only tty:

```toml
[[refclock]]
name = "gps"
type = "gps"
device = "/dev/ttyS0"
baud = 9600
pps = "/dev/pps0"
pps_edge = "assert"
nmea_offset = 0.150
sentences = ["RMC", "ZDA"]
prefer = true
```

Set `pps = "none"` for an NMEA-only receiver. This still provides an
independent stratum-1 source, but with serial sentence-arrival accuracy rather
than kernel PPS accuracy.

Then disable every installed competitor and enable carillon:

```sh
systemctl disable --now ntpd.service chronyd.service systemd-timesyncd.service
systemctl enable --now carillon.service
systemctl --no-pager --full status carillon.service
carillonctl tracking
carillonctl sources
carillonctl refclock
carillonctl serverstats
journalctl -u carillon.service --no-pager -n 50
```

Open UDP/123 in the host firewall only when `[serve]` is enabled. On
firewalld systems:

```sh
firewall-cmd --permanent --add-service=ntp
firewall-cmd --reload
```

Rollback:

```sh
systemctl disable --now carillon.service
systemctl enable --now ntpd.service
```

Substitute the host's former service (`chronyd` or `systemd-timesyncd`) in
the last command when appropriate.

## HTTP monitoring and statistics

Enable the read-only snapshot, health, and Prometheus endpoints on loopback:

```toml
[monitor]
listen = "127.0.0.1:9124"
id = "twocom"
name = "Twocom"
roles = ["colo", "ntp-pool"]
```

The omitted `allow` retains the loopback-only default. After restarting:

```sh
curl --fail http://127.0.0.1:9124/api/v1/status
curl --fail http://127.0.0.1:9124/healthz
curl --fail http://127.0.0.1:9124/metrics
```

For a public NTP host, keep that listener on loopback and use the authenticated
HTTPS example in `apache/carillon-monitor.conf`. Do not expose the full status
document anonymously: it contains source addresses, device names, and recent
errors. For direct LAN access instead, bind the host's private numeric address
and set `monitor.allow` to the LAN prefixes. The daemon checks the immediate
TCP peer and deliberately ignores `X-Forwarded-For`.

Daily tuning data is separate and optional:

```sh
install -d -o carillon -g carillon -m 0750 /var/lib/carillon/stats
```

```toml
[stats]
dir = "/var/lib/carillon/stats"
```

Use `/var/db/carillon/stats` on FreeBSD. Files live under a dated hierarchy in
UTC — `YYYY/MM/DD/loop.tsv`, `YYYY/MM/DD/sources.tsv`, `YYYY/MM/DD/pps.tsv`,
`YYYY/MM/DD/server.tsv` — so no directory collects a year of them and ageing
out is a per-day removal. Set `[stats] keep_days` to have carillon do that
itself; the default of 0 keeps everything.

For a GPS-led stratum-1 host, install a current NIST/IERS
`leap-seconds.list` and set `daemon.leapfile`, even when backup NTP servers are
configured. PPS and the supported NMEA sentences do not supply an advance
leap warning; those backup servers cannot supply it while disconnected. Check
that the file stays valid through the intended offline period. Provision a
current table on NTP-serving hosts as well.

NIST publishes the file at
[`https://tf.nist.gov/leap-seconds.list`](https://tf.nist.gov/leap-seconds.list),
as documented by its
[Internet Time Service](https://www.nist.gov/pml/time-and-frequency-division/time-distribution/internet-time-service-its).
The same validated file can be copied to multiple hosts; a separate NIST
download on every host is unnecessary.

`carillon -check` stays offline and validates the manual file, acquisition
policy, private keys and existing cache. Manual mode checks for replacements
hourly. Missing or expired required data withholds synchronization from the
kernel, NTP server and `waitsync`; time acquisition continues. Monitoring reports
missing data, expiry, conflicts and the 30-day expiry warning.

For automatic updates, configure `[leap] acquire = "nist"` on one seed.
On learners, set `leap_trust = true` on the chosen authenticated `[[server]]`
association; acquisition then defaults to `peers`. Enable redistribution with
`[serve] leap_keys = [<key IDs>]` in addition to the ordinary listener ACL and
keys. Every hop requires explicit trust. Changes to trust and configuration
require a restart; data updates do not. See the
[configuration and wire specification](../docs/leap-distribution.md#configuration).

**Upgrade:** a refclock or serving configuration that lacks a manual file or
an acquisition mode now fails validation. Choose the seed/learner role before
restarting. Empty automatic caches are accepted by `-check`, with service
awaiting UTC acquisition and a durably accepted table. The private cache is
`leap/` beside the drift file. Preserve it across package upgrades and restarts.

## Leap cache recovery

A future UTC bound, a conflicting manual file, or a pending generation that
cannot activate can prevent leap readiness. Diagnose with
`carillonctl -json tracking` and the daemon log first. Fix the time source,
manual file or trust configuration before resetting state; the
[recovery policy](../docs/leap-distribution.md#operator-recovery) describes
when restoring the accepted file or providing a newer generation is enough.

For a full acceptance-record reset, perform these steps as the host's
operator. They remove all rollback, execution and seed-schedule history,
as well as the cached tables. Required-table hosts withhold synchronized
service until acquisition succeeds again. There is no selective reset command.

1. Save the tracking output and relevant logs to the incident record. Record
   the prior accepted/pending hashes and why the reset is necessary. Verify
   the correct UTC against a trusted independent source and resolve any
   armed or just-executed leap before discarding its execution record.
2. Stop the daemon: `systemctl stop carillon.service` on Linux, or
   `service carillon stop` on FreeBSD. Verify it has fully stopped, including
   any manually started instance sharing the cache. Leave the private cache
   directory and `.lock` in place.
3. Locate the cache beside the configured `daemon.drift_file`: normally
   `/var/lib/carillon/leap` on Linux or `/var/db/carillon/leap` on FreeBSD.
   Move `state.json` to a unique audit filename in that same private
   directory. This preserves the old record while removing its canonical
   entry. For example on Linux, replacing the timestamp and reason:

   ```sh
   mv -i /var/lib/carillon/leap/state.json /var/lib/carillon/leap/state.before-reset-20260909T120000Z.json
   sync
   logger -t carillon-leap-reset 'Offline acceptance reset: corrected time source; prior state preserved as state.before-reset-20260909T120000Z.json'
   ```

   Confirm the move completed and `state.json` is absent before continuing.
   Preserve mode 0600 on the audit file and 0700 on the directory. Do not
   remove the configured operator-owned `daemon.leapfile` or drift file.
4. Apply the corrected time/trust configuration and provision current manual
   data or a reachable trusted distributor. Run `carillon -check -config`
   with the actual configuration path; an empty automatic cache reports
   awaiting acquisition. Start with `systemctl start carillon.service` or
   `service carillon start`, then verify `carillonctl tracking`,
   `carillonctl waitsync 120`, health and the activation log. A NIST seed may
   perform a new fetch because its prior check schedule was discarded.

An old audit copy is diagnostic evidence, not a current trust anchor. Restore
it only while the daemon is stopped and after checking that its original
bound and generations are appropriate; restoring it also restores the issue
that prompted the reset if those conditions have not changed.

## FreeBSD with rc.d

Create the daemon account and directories, then install the binaries, rc.d
script, configuration, and optional keys:

```sh
pw groupadd carillon
pw useradd carillon -g carillon -G dialer -d /nonexistent -s /usr/sbin/nologin -c "carillon time daemon"
install -d -o carillon -g carillon -m 0750 /var/db/carillon
install -d -o root -g carillon -m 0750 /usr/local/etc/carillon
install -d -o carillon -g carillon -m 0750 /var/run/carillon
install -o root -g wheel -m 0755 carillon /usr/local/sbin/carillon
install -o root -g wheel -m 0755 carillonctl /usr/local/bin/carillonctl
install -o root -g wheel -m 0555 carillon.rc /usr/local/etc/rc.d/carillon
install -o root -g carillon -m 0640 carillon.toml /usr/local/etc/carillon/carillon.toml
install -o carillon -g carillon -m 0600 keys /usr/local/etc/carillon/keys
pw groupmod carillon -m YOUR_OPERATOR_ACCOUNT
```

The daemon needs the narrow clock and reserved-port privileges supplied by
`mac_ntpd(4)`. Load the policy and set it to carillon's numeric uid; put the
same two settings in `/boot/loader.conf` so they survive reboot:

```sh
kldload mac_ntpd
sysctl security.mac.ntpd.uid="$(id -u carillon)"
sysrc -f /boot/loader.conf mac_ntpd_load=YES
printf 'security.mac.ntpd.uid="%s"\n' "$(id -u carillon)" > /boot/loader.conf.d/carillon.conf
```

For a FreeBSD UART PPS input, use its callout device and enable kernel capture
on the wired modem-control pin. `1` selects CTS and `2` selects DCD; the
example below persists DCD capture on UART 0:

```sh
sysctl dev.uart.0.pps_mode=2
install -d -o root -g wheel -m 0755 /etc/sysctl.conf.d
printf 'dev.uart.0.pps_mode=2\n' > /etc/sysctl.conf.d/carillon-pps.conf
```

Set `device = "/dev/cuau0"` in the `[[refclock]]`. `carillon -check` rejects
a disabled `pps_mode`, and the `dialer` group grants device access.

For a combined GPS receiver, use the same callout tty for NMEA and native
UART PPS, and make the configured pin agree with `pps_mode`:

```toml
[[refclock]]
name = "gps"
type = "gps"
device = "/dev/cuau0"
baud = 9600
pps = "dcd"
pps_edge = "assert"
nmea_offset = 0.150
sentences = ["RMC", "ZDA"]
prefer = true
```

`pps = "cts"` requires the low two `pps_mode` bits to select CTS (`1`);
`pps = "dcd"` requires DCD (`2`).

The rc.d prestart also recreates `/var/run/carillon` after every boot. Adding
operators to the `carillon` group lets them use the mode-`0660` control socket;
they need a fresh login for the group to take effect. The rc.d wrapper sends
the daemon's structured stderr output to syslog with the `carillon` tag.
Validate before stopping ntpd:

```sh
/usr/local/sbin/carillon -check -config /usr/local/etc/carillon/carillon.toml
```

Then make the service replacement persistent:

```sh
sysrc ntpd_enable=NO
sysrc carillon_enable=YES
service ntpd stop
service carillon start
service carillon status
carillonctl tracking
carillonctl sources
carillonctl refclock
carillonctl serverstats
grep carillon /var/log/messages | tail -n 50
```

Rollback:

```sh
service carillon stop
sysrc carillon_enable=NO
sysrc ntpd_enable=YES
service ntpd start
```

## Acceptance and upgrades

After an initial `iburst`, wait for synchronization and query the server path
from another host:

```sh
carillonctl waitsync 30
carillon query server.invalid
carillon query -keys /etc/carillon/keys -key 1 server.invalid
```

Use the platform-appropriate keys path on FreeBSD. An upgrade is an atomic
binary replacement followed by `service carillon restart` or
`systemctl restart carillon`; the drift file preserves the measured frequency.
`carillonctl waitsync 300` may follow the restart directly: it waits for the
control socket to appear. Check `tracking`, `sources`, `serverstats`, and the
service log after every restart.

When several hosts are upgraded, restart the upstream first and let it reach
`synced` before restarting a host that prefers it. A downstream restarted
while its upstream is still settling discards the upstream's stratum-16
replies, synchronizes to its other survivors, and then has to slew back once
the upstream returns; on the in-house pair that cost twocom a 7 ms offset
transient and a frequency excursion of tens of ppm.

The first in-house deployment baseline and its repeatable checks are
recorded in [`ACCEPTANCE.md`](ACCEPTANCE.md).
