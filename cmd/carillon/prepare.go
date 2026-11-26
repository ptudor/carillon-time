package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ptudor/carillon-time/internal/buildinfo"
	"github.com/ptudor/carillon-time/internal/clock"
	"github.com/ptudor/carillon-time/internal/config"
	"github.com/ptudor/carillon-time/internal/leap"
)

func runPrepareLeap(args []string) int {
	fs := flag.NewFlagSet("prepare-leap", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath(), "configuration file")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return exitUsage
	}
	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dir := leap.CacheDir(cfg.Daemon.DriftFile)
	r, err := leap.PrepareSeed(ctx, dir, cfg.LeapMode(), buildinfo.UserAgent(), clock.SynchronizedUTC)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntime
	}
	fmt.Printf("Leap cache ready in %s (%s, SHA-256 %s, expires %s)\n", dir,
		r.Provider.Kind, r.Object.Manifest().Hash(), r.Object.Table().Expiry.UTC().Format(time.RFC3339))
	return 0
}
