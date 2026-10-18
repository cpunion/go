// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
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

func TestIntegerRangeFactory(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" ||
		runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		t.Skip("native coroutine executor is unavailable")
	}
	const source = `package main

import (
 "runtime"
 "sync"
 "time"
)

func bounds(n int, calls *int) int { *calls++; return n }

func signed(n int) int {
 calls, sum := 0, 0
 for i := range bounds(n, &calls) {
  runtime.Gosched()
  if i == 1 { continue }
  if i == 4 { break }
  sum += i
  i = 90
 }
 return calls*100 + sum
}

func unsigned(n uint8) uint64 {
 var sum uint64
 for i := range n { runtime.Gosched(); sum += uint64(i) }
 return sum
}

type count uint16

func named(n count) count {
 var sum count
 for i := range n { runtime.Gosched(); sum += i }
 return sum
}

func assignment(n int) int {
 i, calls, sum := 99, 0, 0
 for i = range bounds(n, &calls) { runtime.Gosched(); sum += i; i = 40 }
 return i*1000 + calls*100 + sum
}

func keyless(n int) int {
 calls, sum := 0, 0
 for range bounds(n, &calls) { runtime.Gosched(); sum++ }
 for _ = range n { runtime.Gosched(); sum++ }
 return calls*100 + sum
}

func nested() int {
 sum := 0
 for i := range 4 {
  for j := range 5 {
   runtime.Gosched()
   if j == 1 { continue }
   if j == 3 { break }
   sum += 10*i+j
  }
  if i == 1 { continue }
  sum++
  if i == 2 { break }
 }
 return sum
}

func pointers() []*int {
 var result []*int
 for i := range 5 { result = append(result, &i); runtime.Gosched(); runtime.GC() }
 return result
}

func early() int {
 for i := range 5 { runtime.Gosched(); if i == 2 { return i } }
 return -1
}

var trace int
func record(i int) { trace = 10*trace+i }
func deferred() {
 for i := range 3 { defer record(i+1); runtime.Gosched() }
}

func worker(wg *sync.WaitGroup, ready, done chan int, value int) {
 defer wg.Done()
 ready <- value
 <-done
}

func spawn(wg *sync.WaitGroup, ready, done chan int) {
 for i := range 128 { wg.Add(1); go worker(wg, ready, done, i) }
 runtime.Gosched()
}

func wait(wg *sync.WaitGroup) { wg.Wait() }

func bubble() { for range 3 { time.Sleep(time.Hour) } }

func captured() []func() int {
 var result []func() int
 for i := range 3 { runtime.Gosched(); result = append(result, func() int { return i }) }
 return result
}

func sliced(values []int) int {
 sum := 0
 for _, value := range values { runtime.Gosched(); sum += value }
 return sum
}

func labeled() int {
 sum := 0
outer:
 for i := range 4 { runtime.Gosched(); if i == 2 { break outer }; sum += i }
 return sum
}

func main() {
 if signed(7) != 105 || signed(0) != 100 || signed(-3) != 100 { panic("signed range") }
 if unsigned(255) != 32385 || unsigned(0) != 0 || named(5) != 10 { panic("typed range") }
 if assignment(3) != 40103 || assignment(0) != 99100 { panic("assigned variable") }
 if keyless(4) != 108 || nested() != 68 || early() != 2 { panic("loop control") }
 for i, value := range pointers() { if *value != i { panic("distinct iteration cell") } }
 deferred()
 if trace != 321 { panic("repeated defer order") }
 var wg sync.WaitGroup
 ready, done := make(chan int, 128), make(chan int)
 baseline := runtime.NumGoroutine()
 spawn(&wg, ready, done)
 var seen [128]bool
 for range 128 { value := <-ready; if seen[value] { panic("duplicate task") }; seen[value] = true }
 if NATIVE && runtime.NumGoroutine() > baseline+8 { panic("integer range spawned ordinary goroutines") }
 runtime.GC()
 close(done)
 wait(&wg)
 for i, f := range captured() { if f() != i { panic("capture fallback") } }
 if sliced([]int{2, 3}) != 5 || labeled() != 1 { panic("unsupported range fallback") }
 println("integer-range-ok")
}
`
	const controller = `package main
import (
 "testing"
 "testing/synctest"
 "time"
)
func TestIntegerRange(t *testing.T) {
 main()
 synctest.Test(t, func(t *testing.T) {
  start := time.Now()
  bubble()
  if time.Since(start) != 3*time.Hour { t.Fatal("bubble time") }
 })
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"disabled", "nocoro", "-l"},
		{"race", "coro", "-l -d=coro=4"},
	} {
		t.Run(config.name, func(t *testing.T) {
			if config.name == "race" {
				testenv.MustHaveCGO(t)
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is unavailable")
				}
			}
			native := config.experiment == "coro" && config.name != "race"
			tmp := t.TempDir()
			file, exe := filepath.Join(tmp, "main.go"), filepath.Join(tmp, "range.exe")
			if err := os.WriteFile(file, []byte(strings.ReplaceAll(source, "NATIVE", strconv.FormatBool(native))), 0o666); err != nil {
				t.Fatal(err)
			}
			testFile := filepath.Join(tmp, "main_test.go")
			if err := os.WriteFile(testFile, []byte(controller), 0o666); err != nil {
				t.Fatal(err)
			}
			args := []string{"test", "-c", "-gcflags=" + config.flags,
				"-gcflags=sync=" + config.flags, "-gcflags=internal/sync=" + config.flags, "-o", exe}
			if config.name == "race" {
				args = append(args, "-race")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file, testFile)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			data, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, symbols)
			}
			for _, name := range []string{"signed", "unsigned", "named", "assignment", "keyless", "nested", "pointers", "early", "deferred", "spawn", "worker", "wait", "bubble"} {
				if got := strings.Contains(string(symbols), "main."+name+".coro"); got != native {
					t.Errorf("%s native entry = %t, want %t\n%s", name, got, native, data)
				}
			}
			for _, name := range []string{"captured", "sliced", "labeled"} {
				if strings.Contains(string(symbols), "main."+name+".coro") {
					t.Errorf("%s lost conservative range fallback", name)
				}
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				run := testenv.CommandContext(t, ctx, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs), "GOGC=10")
				data, err := run.CombinedOutput()
				cancel()
				if err != nil || string(data) != "integer-range-ok\nPASS\n" {
					t.Errorf("P%d: %v\n%s", procs, err, data)
				}
			}
		})
	}
}
