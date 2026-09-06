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
`leap-seconds.list` and set `daemon.leapfile`. `carillon -check` parses the
file, warns 30 days before expiry, and warns when it is expired. Monitoring
reports an expiring file as degraded and an expired file as unhealthy. The
file overrides survivor leap bits and drives the kernel insertion/deletion
flag only during the final UTC day.

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
