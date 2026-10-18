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

func TestWaitGroup(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" ||
		runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("native coroutine executor is unavailable")
	}
	const source = `package sync

import "runtime"

const CoroWaitGroupNative = false

func coroWaitGroupWait(wg *WaitGroup) { wg.Wait() }
func coroWaitGroupDone(wg *WaitGroup) { wg.Done() }
func coroWaitGroupCycle(wg *WaitGroup, request, completed chan int) {
 for i := 0; i < 100; i++ {
  wg.Add(2)
  request <- i
  wg.Wait()
  completed <- i
 }
}

var CoroWaitGroupWait = coroWaitGroupWait
var CoroWaitGroupDone = coroWaitGroupDone
var CoroWaitGroupCycle = coroWaitGroupCycle

func coroWaitGroupDeferred(wg *WaitGroup) {
 defer wg.Done()
 runtime.Gosched()
}
func coroWaitGroupDeferredPanic(wg *WaitGroup) {
 defer wg.Done()
 runtime.Gosched()
 panic("deferred waitgroup")
}
func coroWaitGroupDeferredExit(wg *WaitGroup) {
 defer wg.Done()
 runtime.Gosched()
 runtime.Goexit()
}
func coroWaitGroupDeferredRecover(wg *WaitGroup) (value any) {
 defer func() { value = recover() }()
 defer wg.Done()
 runtime.Gosched()
 return nil
}
var CoroWaitGroupDeferred = coroWaitGroupDeferred
var CoroWaitGroupDeferredPanic = coroWaitGroupDeferredPanic
var CoroWaitGroupDeferredExit = coroWaitGroupDeferredExit
var CoroWaitGroupDeferredRecover = coroWaitGroupDeferredRecover

func coroWaitGroupWorker(wg *WaitGroup, started chan struct{}, release <-chan struct{}, mode int) {
 defer wg.Done()
 defer func() {
  if value := recover(); value != nil && value != "spawn recover" { panic(value) }
 }()
 runtime.Gosched()
 started <- struct{}{}
 <-release
 if mode == 1 { panic("spawn recover") }
 if mode == 2 { runtime.Goexit() }
}
func coroWaitGroupSpawn(wg *WaitGroup, started chan struct{}, release <-chan struct{}, count, mode int) {
 runtime.Gosched()
 for i := 0; i < count; i++ {
  go coroWaitGroupWorker(wg, started, release, mode)
 }
}
var CoroWaitGroupSpawn = coroWaitGroupSpawn

func CoroWaitGroupWaiters(wg *WaitGroup) uint32 {
 return uint32(wg.state.Load() & 0x7fffffff)
}
`
	const controller = `package sync_test

import (
 "runtime"
 "sync"
 "testing"
 "testing/synctest"
 "time"
)

func TestCoroWaitGroupEntry(t *testing.T) {
 // Keep the test driver ordinary; each exported fixture enters through its
 // public Go entry, not a hand-written call to a private factory.
 for range 1 {
  var wg sync.WaitGroup
  sync.CoroWaitGroupWait(&wg)
  request, completed := make(chan int), make(chan int)
  producer := make(chan struct{})
  go func() {
   for range 100 {
    <-request
    // Do not release until the native Wait has registered a waiter. This
    // exercises suspension rather than just the zero-counter fast path.
    for sync.CoroWaitGroupWaiters(&wg) == 0 { runtime.Gosched() }
    wg.Done()
    runtime.GC()
    sync.CoroWaitGroupDone(&wg)
    <-completed
   }
   close(producer)
  }()
  sync.CoroWaitGroupCycle(&wg, request, completed)
  <-producer
  if sync.CoroWaitGroupWaiters(&wg) != 0 { t.Fatal("retained waiter") }
  sync.CoroWaitGroupWait(&wg)
 }
}

func TestCoroWaitGroupBubble(t *testing.T) {
 synctest.Test(t, func(t *testing.T) {
  var wg sync.WaitGroup
  wg.Add(1)
  start := time.Now()
  go func() {
   time.Sleep(time.Hour)
   sync.CoroWaitGroupDone(&wg)
  }()
  sync.CoroWaitGroupWait(&wg)
  if time.Since(start) != time.Hour { t.Fatal("lost durable wait accounting") }
  wg.Add(1)
  sync.CoroWaitGroupDeferred(&wg)
  wg.Wait()
  synctest.Wait()
 })
}

func TestCoroWaitGroupDeferred(t *testing.T) {
 for range 1 {
  var wg sync.WaitGroup
  wg.Add(1)
  sync.CoroWaitGroupDeferred(&wg)
  wg.Wait()
  wg.Add(1)
  func() {
   defer func() {
    if v := recover(); v != "deferred waitgroup" { t.Errorf("panic=%v", v) }
   }()
   sync.CoroWaitGroupDeferredPanic(&wg)
   t.Error("panic returned")
  }()
  wg.Wait()
  exited := make(chan struct{})
  wg.Add(1)
  go func() {
   defer close(exited)
   sync.CoroWaitGroupDeferredExit(&wg)
   t.Error("Goexit returned")
  }()
  wg.Wait()
  <-exited
  if v := sync.CoroWaitGroupDeferredRecover(&wg); v != "sync: negative WaitGroup counter" {
   t.Errorf("negative counter panic=%v", v)
  }
 }
}

func TestCoroWaitGroupSpawn(t *testing.T) {
 const count = 128
 for mode := 0; mode < 3; mode++ {
  var wg sync.WaitGroup
  started, release := make(chan struct{}, count), make(chan struct{})
  wg.Add(count)
  before := runtime.NumGoroutine()
  sync.CoroWaitGroupSpawn(&wg, started, release, count, mode)
  for i := 0; i < count; i++ { <-started }
  parked := runtime.NumGoroutine()
  runtime.GC()
  close(release)
  wg.Wait()
  if sync.CoroWaitGroupNative && parked > before+32 {
   t.Fatalf("mode %d created ordinary goroutines: before=%d parked=%d", mode, before, parked)
  }
 }
}
`
	for _, config := range []struct {
		name, experiment, flags string
		race                    bool
	}{
		{"inline", "coro", "-d=coro=4", false},
		{"noinline", "coro", "-l -d=coro=4", false},
		{"checkptr", "coro", "-d=coro=4 -d=checkptr=2", false},
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
			tmp := t.TempDir()
			replacements := make(map[string]string)
			for name, text := range map[string]string{
				"coro_waitgroup_entry_test.go":      source,
				"coro_waitgroup_controller_test.go": controller,
			} {
				if name == "coro_waitgroup_entry_test.go" {
					native := config.experiment == "coro" && !config.race
					text = strings.Replace(text, "const CoroWaitGroupNative = false",
						"const CoroWaitGroupNative = "+strconv.FormatBool(native), 1)
				}
				fixture := filepath.Join(tmp, name)
				if err := os.WriteFile(fixture, []byte(text), 0o666); err != nil {
					t.Fatal(err)
				}
				replacements[filepath.Join(runtime.GOROOT(), "src", "sync", name)] = fixture
			}
			data, err := json.Marshal(struct{ Replace map[string]string }{replacements})
			if err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(tmp, "overlay.json")
			if err := os.WriteFile(overlay, data, 0o666); err != nil {
				t.Fatal(err)
			}
			exe := filepath.Join(tmp, "waitgroup.test")
			args := []string{"test", "-c", "-overlay=" + overlay,
				"-gcflags=sync=" + config.flags, "-o", exe, "sync"}
			if config.race {
				args = append(args, "-race")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			data, err = build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" && !config.race {
				nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
				symbols, err := nm.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, symbols)
				}
				for _, name := range []string{"Wait", "Done", "Cycle", "Deferred", "DeferredPanic", "DeferredExit", "DeferredRecover", "Spawn", "Worker"} {
					if !strings.Contains(string(symbols), "sync.coroWaitGroup"+name+".coro") {
						var diagnostics []string
						for _, line := range strings.Split(string(data), "\n") {
							if strings.Contains(line, "WaitGroup") {
								diagnostics = append(diagnostics, line)
							}
						}
						t.Fatalf("missing native coroWaitGroup%s\n%s", name, strings.Join(diagnostics, "\n"))
					}
				}
				if !strings.Contains(string(symbols), "runtime.coroSemacquireWaitGroup") {
					t.Fatal("missing native WaitGroup semaphore entry")
				}
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				run := testenv.CommandContext(t, ctx, exe, "-test.run=^TestCoroWaitGroup", "-test.count=3")
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
