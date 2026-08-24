# Archived OpenWrt prototype

The Go/OpenWrt target was deliberately deferred on 2026-08-23: the static Go
binary and runtime are too large for the intended routers. OpenWrt time/PPS
support will be a separate C project with its own footprint and acceptance
criteria.

`carillon.init` is retained only as the earlier procd sketch. It is not a
supported deployment artifact, and the root Makefile no longer advertises or
builds OpenWrt binaries.
