// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"encoding/json"
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNotifyListFactory(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" ||
		runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("native coroutine executor is unavailable")
	}
	const source = `package sync

import "runtime"

func coroNotifyWait(c *Cond, ticket uint32) { runtime_notifyListWait(&c.notify, ticket) }
func coroNotifyWorker(c *Cond, ready, done chan struct{}) {
 ticket := runtime_notifyListAdd(&c.notify)
 ready <- struct{}{}
 runtime_notifyListWait(&c.notify, ticket)
 done <- struct{}{}
}
func coroNotifySpawn(c *Cond, ready, done chan struct{}) {
 for i := 0; i < 128; i++ { go coroNotifyWorker(c, ready, done) }
 runtime.Gosched()
}
func coroNotifyPublicWait(c *Cond) { c.Wait() }

var CoroNotifyWait = coroNotifyWait
var CoroNotifySpawn = coroNotifySpawn
var CoroNotifyPublicWait = coroNotifyPublicWait

func CoroNotifyAdd(c *Cond) uint32 { return runtime_notifyListAdd(&c.notify) }
`
	const controller = `package sync_test

import (
 "runtime"
 "sync"
 "testing"
 "testing/synctest"
 "time"
)

func TestCoroNotifyEntry(t *testing.T) {
 for range 1 {
  var mu sync.Mutex
  c := sync.NewCond(&mu)
  ticket := sync.CoroNotifyAdd(c)
  c.Signal()
  sync.CoroNotifyWait(c, ticket)
  ready, done := make(chan struct{}, 128), make(chan struct{}, 128)
  baseline := runtime.NumGoroutine()
  sync.CoroNotifySpawn(c, ready, done)
  for i := 0; i < 128; i++ { <-ready }
  if NATIVE && runtime.NumGoroutine() > baseline+8 {
   t.Fatal("logical notification waiters created ordinary goroutines")
  }
  runtime.GC()
  c.Broadcast()
  for i := 0; i < 128; i++ { <-done }

  // The Locker interface is still unsupported. Keep the ordinary body so
  // that public Cond.Wait, including lock reacquisition, remains correct.
  started, finished := make(chan struct{}), make(chan struct{})
  go func() {
   mu.Lock()
   close(started)
   sync.CoroNotifyPublicWait(c)
   mu.Unlock()
   close(finished)
  }()
  <-started
  mu.Lock()
  c.Signal()
  mu.Unlock()
  <-finished
 }
}

func TestCoroNotifyBubble(t *testing.T) {
 synctest.Test(t, func(t *testing.T) {
  var mu sync.Mutex
  c := sync.NewCond(&mu)
  ticket := sync.CoroNotifyAdd(c)
  start := time.Now()
  go func() { time.Sleep(time.Hour); c.Broadcast() }()
  sync.CoroNotifyWait(c, ticket)
  if time.Since(start) != time.Hour { t.Fatal("lost bubble wait accounting") }
 })
}
`
	for _, config := range []struct {
		name, experiment, flags string
		race                    bool
	}{
		{"inline", "coro", "-d=coro=4", false},
		{"noinline", "coro", "-l -d=coro=4", false},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", false},
		{"disabled", "nocoro", "-l", false},
		{"race", "coro", "-d=coro=4", true},
	} {
		t.Run(config.name, func(t *testing.T) {
			if config.race {
				testenv.MustHaveCGO(t)
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is unavailable")
				}
			}
			native := config.experiment == "coro" && !config.race
			tmp := t.TempDir()
			replacements := make(map[string]string)
			for name, text := range map[string]string{
				"coro_notify_entry_test.go":      source,
				"coro_notify_controller_test.go": strings.ReplaceAll(controller, "NATIVE", strconv.FormatBool(native)),
			} {
				file := filepath.Join(tmp, name)
				if err := os.WriteFile(file, []byte(text), 0o666); err != nil {
					t.Fatal(err)
				}
				replacements[filepath.Join(runtime.GOROOT(), "src", "sync", name)] = file
			}
			data, err := json.Marshal(struct{ Replace map[string]string }{replacements})
			if err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(tmp, "overlay.json")
			if err := os.WriteFile(overlay, data, 0o666); err != nil {
				t.Fatal(err)
			}
			exe := filepath.Join(tmp, "notify.test")
			args := []string{"test", "-c", "-overlay=" + overlay,
				"-gcflags=sync=" + config.flags, "-o", exe, "sync"}
			if config.race {
				args = append(args, "-race")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err = build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, symbols)
			}
			for _, name := range []string{"Wait", "Worker", "Spawn"} {
				if got := strings.Contains(string(symbols), "sync.coroNotify"+name+".coro"); got != native {
					t.Fatalf("coroNotify%s native entry = %t, want %t\n%s", name, got, native, data)
				}
			}
			if strings.Contains(string(symbols), "sync.coroNotifyPublicWait.coro") {
				t.Fatal("dynamic Locker wait lost its conservative fallback")
			}
			if got := strings.Contains(string(symbols), "runtime.coroNotifyListWait"); got != native {
				t.Fatalf("runtime notification entry = %t, want %t", got, native)
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				run := testenv.CommandContext(t, ctx, exe, "-test.run=^TestCoroNotify", "-test.count=3")
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs))
				data, err := run.CombinedOutput()
				cancel()
				if err != nil || string(data) != "PASS\n" {
					t.Errorf("P%d: %v\n%s", procs, err, data)
				}
			}
		})
	}
}
