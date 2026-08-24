//go:build !freebsd

package config

func checkRefclockPlatform(r *Refclock) error { return nil }
