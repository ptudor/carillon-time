# Linux installation and operations

The `carillon` RPM and DEB contain both `carillon` and `carillonctl`. They target
systemd-based Linux on amd64 and arm64. CI exercises Debian 13 and Fedora 43 on
amd64; other distribution versions need their own acceptance run.

## Package layout and policy

| Path | Purpose |
| --- | --- |
| `/usr/bin/carillon`, `/usr/bin/carillonctl` | Static Go executables |
| `/etc/carillon/carillon.toml` | Root-owned `0640` configuration, group `carillon` |
| `/var/lib/carillon` | Persistent state, owned by `carillon`, mode `0750` |
| `/run/carillon` | Runtime/control-socket directory created by systemd |
| `/usr/lib/systemd/system/carillon.service` | Vendor unit; customize with drop-ins |

The package creates a dedicated account and configuration. **It never enables,
starts, or restarts the daemon, and never disables another time service.** The
packaged configuration polls three NTP Pool entries. It does not serve clients,
enable HTTP monitoring, or configure reference-clock devices.

Production binaries have no cgo dependency. The unit grants only `CAP_SYS_TIME`
and `CAP_NET_BIND_SERVICE`, isolates the filesystem, and hides devices by default.
The manual installation unit under `deploy/systemd/` uses `/usr/local` paths and
includes example serial-device access; it is a separate installation recipe.

## Install and prepare

[Verify your download](releases.md#verify-a-download), then install the package:

```sh
sudo apt install ./carillon_*_amd64.deb
# Or on an RPM system:
sudo dnf --setopt=localpkg_gpgcheck=0 install ./carillon-*.x86_64.rpm
```

Choose `arm64` DEBs or `aarch64` RPMs for ARM. The local packages are unsigned.
Do not disable signature checks globally; the DNF option above applies only to
local-package checks for that invocation.

Edit `/etc/carillon/carillon.toml` to choose your upstreams and topology. The
[full example](../deploy/carillon.toml.example) covers serving, authentication,
GPS/PPS, monitoring, leapfiles, and statistics.

```sh
sudo systemd-tmpfiles --create /usr/lib/tmpfiles.d/carillon.conf
sudo -u carillon /usr/bin/carillon -check -config /etc/carillon/carillon.toml
```

The tmpfiles command ensures the control-socket directory exists even after a
service stop has removed it. The preflight check exits before opening the system
clock. It does not prove that upstreams,
devices, or clock permissions work. Keep your existing time daemon running while
preparing configuration.

## Activate on the intended host

Identify the existing service (`chronyd`, `chrony`, `ntpd`, `ntp`, `ntpsec`, or
`systemd-timesyncd`). Stop and disable **the service actually in use**, then enable
Carillon. For example, on a host using `chronyd`:

```sh
sudo systemctl disable --now chronyd.service
sudo systemctl enable --now carillon.service
sudo carillonctl waitsync 120
sudo carillonctl tracking
sudo carillonctl sources
sudo journalctl -u carillon -n 50 --no-pager
```

Starting the daemon can adjust the host clock. Do this in your intended maintenance
window. The unit conflicts with common time services, and Carillon also checks
whether UDP/123 is already occupied. Only one process should discipline the clock.
If activation fails, inspect logs and restore your previous time service as needed.

Operators can be added to the `carillon` group to read the local control socket
without `sudo`. Group membership also grants read access to the configuration;
new membership takes effect in a new login session.

For authenticated NTP, put shared keys in a separate file owned by `carillon`
with mode `0600`, and set `daemon.keys`. The daemon’s Unix socket access and MAC key
permissions are separate concerns. Never place real keys in a bug report or commit.

## GPS and PPS device access

Hardware acceptance is still pending; read [known limitations](limitations.md).
The package’s network-only service has `PrivateDevices=yes`. To use an existing
Linux `/dev/pps0`, first arrange read/write access for the daemon with a suitable
udev rule or device group. Then create a drop-in with `sudo systemctl edit carillon`:

```ini
[Service]
PrivateDevices=no
DevicePolicy=closed
DeviceAllow=/dev/pps0 rw
```

Add `SupplementaryGroups=` for the actual device-owning group when needed. Debian
often uses `dialout` for serial ports; inspect your distribution’s device ownership.
For NMEA on a serial port, add that exact device, for example
`DeviceAllow=/dev/ttyS0 rw`, and configure its permissions too.

Linux serial PPS needs the appropriate kernel PPS support; the `pps_ldisc` line
discipline applies to a PPS-only tty. NMEA on Linux needs a separate readable
serial stream from a tty used for `N_PPS`. Loading modules and hardware/udev setup
are administrator tasks, not package-install actions. Restart after changing the
drop-in, and inspect `carillonctl refclock` plus service logs.

## Upgrade, rollback, and removal

Back up configuration, shared keys, and persistent state. Read release notes and
verify the new download. Upgrade through APT or DNF; the configured conffile is
preserved (`%config(noreplace)` on RPM). Review `.rpmnew` files or Debian conffile
prompts. Validate configuration with the newly installed executable, then
explicitly restart Carillon and check tracking, sources, and logs. The old process
continues until restarted.

Keep the previous package and matching backup if you need a rollback. Stop
Carillon before restoring files or switching time daemons; do not run two
discipline loops while investigating a failure.

`apt remove carillon` or `dnf remove carillon` stops and disables the service when
systemd is active. It preserves the account and generated state. Debian `apt purge`
also removes package-managed configuration. Re-enable your previous time service
explicitly if the host still needs synchronization.

For a source-installed binary, `command -v carillon` may still find
`/usr/local/sbin/carillon` ahead of the packaged executable. Check paths during
migration; the package unit always invokes `/usr/bin/carillon`.
