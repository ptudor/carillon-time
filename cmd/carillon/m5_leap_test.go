package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/carillon-time/internal/leap"
)

func TestOfflineCheckValidatesExistingCacheAndDatedManualFile(t *testing.T) {
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, kind := range []string{"empty automatic cache", "existing cache", "corrupt cache", "undated manual file"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "leap")
			data := []byte("#$ 3676924800\n#@ 4007404800\n2272060800 10\n2287785600 11\n")
			var before []byte
			if kind != "empty automatic cache" {
				o, err := leap.NewObject(data)
				if err != nil {
					t.Fatal(err)
				}
				store, err := leap.OpenStore(cache)
				if err != nil {
					t.Fatal(err)
				}
				err = store.Save(leap.State{Active: &leap.Record{Object: o, Provider: leap.Provider{Kind: "file", Name: "local"}, Accepted: now}, UTCbound: now})
				closeErr := store.Close()
				if err != nil || closeErr != nil {
					t.Fatalf("fixture cache: %v %v", err, closeErr)
				}
				if kind == "corrupt cache" {
					if err := os.WriteFile(filepath.Join(cache, "state.json"), []byte(`{"version":1,"bad":true}`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				before, err = os.ReadFile(filepath.Join(cache, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf("[daemon]\ndrift_file=%q\ncontrol=%q\n", filepath.Join(dir, "drift"), filepath.Join(dir, "control"))
			if kind == "undated manual file" {
				manual := filepath.Join(dir, "manual.list")
				if err := os.WriteFile(manual, []byte(strings.SplitN(string(data), "\n", 2)[1]), 0600); err != nil {
					t.Fatal(err)
				}
				body += fmt.Sprintf("leapfile=%q\n", manual)
			} else {
				body += "[leap]\nacquire='nist'\n"
			}
			body += "[[server]]\naddress='pool.ntp.org'\n"
			path := filepath.Join(dir, "carillon.toml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "corrupt cache" || kind == "undated manual file" {
				want = exitUsage
			}
			if code := runDaemon([]string{"-check", "-config", path}); code != want {
				t.Fatalf("-check exit %d, want %d", code, want)
			}
			if before != nil {
				after, err := os.ReadFile(filepath.Join(cache, "state.json"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("offline check modified existing state")
				}
			} else if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Fatalf("offline check created a cache: %v", err)
			}
		})
	}
}
