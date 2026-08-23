// Package buildinfo exposes the version and build time stamped into the
// binary by the Makefile (-ldflags -X). When built without them, Version is
// "dev" and BuildTime falls back to the VCS commit time recorded by the Go
// toolchain, so the "no date earlier than the build" sanity checks still have
// something to work with.
package buildinfo

import (
	"runtime/debug"
	"time"
)

// Version is the human-readable version string, set at link time.
var Version = "dev"

// BuildTime is the UTC build timestamp in RFC 3339 form, set at link time.
var BuildTime = ""

// Time returns the build time. It prefers the linker-provided value, then the
// VCS commit time from the module build info, and finally the zero time when
// neither is available (callers treat zero as "unknown", not as 1970).
func Time() time.Time {
	if BuildTime != "" {
		if t, err := time.Parse(time.RFC3339, BuildTime); err == nil {
			return t.UTC()
		}
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.time" {
				if t, err := time.Parse(time.RFC3339, s.Value); err == nil {
					return t.UTC()
				}
			}
		}
	}
	return time.Time{}
}
