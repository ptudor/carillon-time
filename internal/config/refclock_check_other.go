//go:build !freebsd && !linux

package config

func checkRefclockPlatform(r *Refclock) error { return nil }
