# carillon deployment quickstart

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

Then disable every installed competitor and enable carillon:

```sh
systemctl disable --now ntpd.service chronyd.service systemd-timesyncd.service
systemctl enable --now carillon.service
systemctl --no-pager --full status carillon.service
carillonctl tracking
carillonctl sources
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
carillon query server.example.net
carillon query -keys /etc/carillon/keys -key 1 server.example.net
```

Use the platform-appropriate keys path on FreeBSD. An upgrade is an atomic
binary replacement followed by `service carillon restart` or
`systemctl restart carillon`; the drift file preserves the measured frequency.
Check `tracking`, `sources`, `serverstats`, and the service log after every
restart. The first in-house deployment baseline and its repeatable checks are
recorded in [`ACCEPTANCE.md`](ACCEPTANCE.md).
