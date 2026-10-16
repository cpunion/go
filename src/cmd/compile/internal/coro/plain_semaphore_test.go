// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"encoding/json"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPlainSemaphore(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" ||
		runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("native coroutine executor is unavailable")
	}
	const source = `package PACKAGE

import "runtime"

func coroPlainAcquire(addr *uint32) { runtime_Semacquire(addr) }
func coroPlainRelease(addr *uint32) { RELEASE }

func coroPlainWaiter(addr *uint32, ready, done chan struct{}) {
 ready <- struct{}{}
 runtime.Gosched()
 runtime_Semacquire(addr)
 done <- struct{}{}
}

func coroPlainSpawn(addr *uint32, ready, done chan struct{}) {
 for i := 0; i < 128; i++ { go coroPlainWaiter(addr, ready, done) }
 runtime.Gosched()
}

func coroPlainReleaseMany(addr *uint32) {
 for i := 0; i < 129; i++ { coroPlainRelease(addr) }
}

var CoroPlainAcquire = coroPlainAcquire
var CoroPlainRelease = coroPlainRelease
var CoroPlainSpawn = coroPlainSpawn
var CoroPlainReleaseMany = coroPlainReleaseMany

func CoroPlainOrdinaryWait(addr *uint32, ready, done chan struct{}) {
 for range 1 {
  ready <- struct{}{}
  runtime_Semacquire(addr)
  done <- struct{}{}
 }
}
`
	const controller = `package PACKAGE_test

import (
 semaphore "PACKAGEPATH"
 "runtime"
 "testing"
)

func TestCoroPlainSemaphoreEntry(t *testing.T) {
 // The controller stays ordinary, as in the public Mutex/RWMutex tests.
 for range 1 {
  var sema uint32
  semaphore.CoroPlainRelease(&sema)
  semaphore.CoroPlainAcquire(&sema)
  if sema != 0 { t.Fatal("ready acquisition did not consume its token") }
  ready, done := make(chan struct{}, 129), make(chan struct{}, 129)
  baseline := runtime.NumGoroutine()
  go semaphore.CoroPlainOrdinaryWait(&sema, ready, done)
  semaphore.CoroPlainSpawn(&sema, ready, done)
  for i := 0; i < 129; i++ { <-ready }
  if NATIVE && runtime.NumGoroutine() > baseline+8 {
   t.Fatal("logical semaphore waiters created ordinary goroutines")
  }
  runtime.GC()
  semaphore.CoroPlainReleaseMany(&sema)
  for i := 0; i < 129; i++ { <-done }
  if sema != 0 { t.Fatal("completed acquisitions retained a token") }
 }
}
`
	for _, pkg := range []string{"sync", "internal/poll"} {
		t.Run(pkg, func(t *testing.T) {
			tmp := t.TempDir()
			for _, config := range []struct{ name, experiment, flags string }{
				{"inline", "coro", "-d=coro=4"},
				{"noinline", "coro", "-l -d=coro=4"},
				{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
				{"disabled", "nocoro", "-l"},
			} {
				t.Run(config.name, func(t *testing.T) {
					release := "runtime_Semrelease(addr)"
					if pkg == "sync" {
						release = "runtime_Semrelease(addr, false, 0)"
					}
					text := strings.NewReplacer("PACKAGE", filepath.Base(pkg),
						"RELEASE", release).Replace(source)
					fixture := filepath.Join(tmp, config.name+"_test.go")
					if err := os.WriteFile(fixture, []byte(text), 0o666); err != nil {
						t.Fatal(err)
					}
					external := filepath.Join(tmp, config.name+"_external_test.go")
					text = strings.NewReplacer("PACKAGEPATH", pkg, "PACKAGE", filepath.Base(pkg),
						"NATIVE", strconv.FormatBool(config.experiment == "coro")).Replace(controller)
					if err := os.WriteFile(external, []byte(text), 0o666); err != nil {
						t.Fatal(err)
					}
					overlay := filepath.Join(tmp, config.name+".json")
					data, err := json.Marshal(struct{ Replace map[string]string }{
						map[string]string{
							filepath.Join(runtime.GOROOT(), "src", pkg, "coro_plain_entry_test.go"):      fixture,
							filepath.Join(runtime.GOROOT(), "src", pkg, "coro_plain_controller_test.go"): external,
						},
					})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(overlay, data, 0o666); err != nil {
						t.Fatal(err)
					}
					exe := filepath.Join(tmp, config.name+".test")
					build := testenv.Command(t, testenv.GoToolPath(t), "test", "-c", "-overlay="+overlay,
						"-gcflags="+pkg+"="+config.flags, "-o", exe, pkg)
					build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
					data, err = build.CombinedOutput()
					if err != nil {
						t.Fatalf("build: %v\n%s", err, data)
					}
					if config.experiment == "coro" {
						nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
						symbols, err := nm.CombinedOutput()
						if err != nil {
							t.Fatalf("nm: %v\n%s", err, symbols)
						}
						for _, name := range []string{"Acquire", "Release", "Waiter", "Spawn", "ReleaseMany"} {
							if !strings.Contains(string(symbols), pkg+".coroPlain"+name+".coro") {
								var diagnostics []string
								for _, line := range strings.Split(string(data), "\n") {
									if strings.Contains(line, "coroPlain") {
										diagnostics = append(diagnostics, line)
									}
								}
								t.Fatalf("missing native %s.coroPlain%s\n%s", pkg, name, strings.Join(diagnostics, "\n"))
							}
						}
					}
					for _, procs := range []int{1, 4} {
						ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
						run := testenv.CommandContext(t, ctx, exe,
							"-test.run=^TestCoroPlainSemaphoreEntry$", "-test.count=3")
						run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs))
						data, err := run.CombinedOutput()
						cancel()
						if err != nil || string(data) != "PASS\n" {
							t.Errorf("P%d: %v\n%s", procs, err, data)
						}
					}
				})
			}
		})
	}
}
