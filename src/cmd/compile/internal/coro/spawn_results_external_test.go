// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSpawnResults(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
		runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is unavailable")
	}
	files := map[string]string{
		"go.mod": "module example.com/spawnresults\n\ngo 1.28\n",
		"helper/helper.go": `package helper

import "runtime"

type Box struct { Value int }

func Pair(index int, p *Box, ready chan<- int, gate <-chan struct{}, out *int, done chan struct{}) (int, *Box) {
 ready <- index
 <-gate
 runtime.Gosched()
 *out = p.Value
 close(done)
 return p.Value, p
}

func (b Box) Pair(index int, ready chan<- int, gate <-chan struct{}, out *int, done chan struct{}) (int, *Box) {
 ready <- index
 <-gate
 runtime.Gosched()
 *out = b.Value
 close(done)
 return b.Value, &b
}

func Named(index int, p *Box, ready chan<- int, gate <-chan struct{}, out *int, done chan struct{}) (value int, pointer *Box) {
 defer func() { value++; *out = value; close(done) }()
 ready <- index
 <-gate
 runtime.Gosched()
 value, pointer = p.Value, p
 return
}

func Exit(index int, p *Box, ready chan<- int, gate <-chan struct{}, out *int, done chan struct{}) (int, *Box) {
 defer func() { close(done) }()
 ready <- index
 <-gate
 *out = p.Value
 runtime.Goexit()
 return 0, nil
}

func Panic(implicit bool) (int, *Box) {
 runtime.Gosched()
 if implicit { var p *Box; return p.Value, p }
 panic("spawn-result-panic")
}

func Single(value int) int { runtime.Gosched(); return value }

// No local go statement demands a factory for this exported entry.
func Plain(out *int, done chan struct{}) int { *out = 29; close(done); return 29 }
`,
		"main.go": `package main

import (
 "example.com/spawnresults/helper"
 "os"
 "runtime"
)

var next int
var failArgument bool

func argument() int {
 if failArgument { panic(42) }
 value := next
 next++
 return value
}

func spawn(ready chan<- int, gate <-chan struct{}, results *[128]int, done *[128]chan struct{}) {
 for i := range 128 {
  p := &helper.Box{Value: i}
  index := argument()
  if i%4 == 0 {
   go helper.Pair(index, p, ready, gate, &results[i], done[i])
  } else if i%4 == 1 {
   go p.Pair(index, ready, gate, &results[i], done[i]); p.Value = -1
  } else if i%4 == 2 {
   go helper.Named(index, p, ready, gate, &results[i], done[i])
  } else {
   go helper.Exit(index, p, ready, gate, &results[i], done[i])
  }
 }
}

func plain(out *int, done chan struct{}) { go helper.Plain(out, done) }

func fatal(implicit bool) { go helper.Panic(implicit) }

func argumentSpawn() { go helper.Single(argument()) }

func main() {
 if os.Getenv("ARGUMENT_PANIC") != "" {
  failArgument = true
  defer func() {
   if recover() != 42 || next != 0 { panic("argument panic changed") }
   println("spawn-result-argument-ok")
  }()
  argumentSpawn()
  panic("argument did not panic")
 }
 var results [128]int
 var done [128]chan struct{}
 for i := range 128 { done[i] = make(chan struct{}) }
 ready, gate, finished := make(chan int, 128), make(chan struct{}), make(chan struct{})
 baseline := runtime.NumGoroutine()
 // Keep this controller ordinary: the creator must return while its
 // independent children remain parked with their discarded result slots.
 go func() { spawn(ready, gate, &results, &done); close(finished) }()
 <-finished
 var seen [128]bool
 for range 128 {
  index := <-ready
  if index < 0 || index >= 128 || seen[index] { panic("argument snapshot") }
  seen[index] = true
 }
 if next != 128 { panic("argument evaluation count") }
 if os.Getenv("EXPECT_NATIVE") == "1" && runtime.NumGoroutine() > baseline+12 {
  panic("discarded results retained ordinary goroutines")
 }
 runtime.GC()
 close(gate)
 for i := range 128 {
  <-done[i]
  want := i
  if i%4 == 2 { want++ }
  if results[i] != want { panic("result root, receiver snapshot, defer, or Goexit") }
 }
 out, signal := 0, make(chan struct{})
 plain(&out, signal)
 <-signal
 if out != 29 { panic("missing plain adapter fallback") }
 if mode := os.Getenv("PANIC"); mode != "" {
  defer func() { if recover() != nil { println("parent-recovered-child") } }()
  fatal(mode == "implicit")
  runtime.Gosched()
  select {}
 }
 println("spawn-results-ok")
}
`,
	}
	for _, mode := range []string{"inline", "noinline", "checkptr", "missing-factory", "race", "nocoro"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "race" {
				testenv.MustHaveCGO(t)
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is unavailable")
				}
			}
			dir := t.TempDir()
			for name, contents := range files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
					t.Fatal(err)
				}
			}
			exe := filepath.Join(dir, "spawn-results")
			flags, experiment := "-d=coro=4", "coro"
			if mode != "inline" {
				flags += " -l"
			}
			if mode == "nocoro" {
				flags, experiment = "-l", "nocoro"
			}
			args := []string{"build", "-o", exe, "-gcflags=example.com/spawnresults/...=" + flags}
			if mode == "checkptr" {
				args = append(args, "-gcflags=all=-d=checkptr=2",
					"-gcflags=example.com/spawnresults/...="+flags+" -d=checkptr=2")
			}
			if mode == "missing-factory" {
				args = append(args, "-gcflags=example.com/spawnresults/helper=-l -d=coro=1")
			}
			if mode == "race" {
				args = append(args, "-race")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, ".")...)
			build.Dir = dir
			build.Env = append(build.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			t.Logf("compiler diagnostics:\n%s", out)
			native := mode == "inline" || mode == "noinline" || mode == "checkptr"
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, symbols)
			}
			for _, name := range []string{"main.spawn", "main.fatal", "main.argumentSpawn", "example.com/spawnresults/helper.Pair", "example.com/spawnresults/helper.Box.Pair", "example.com/spawnresults/helper.Named", "example.com/spawnresults/helper.Exit", "example.com/spawnresults/helper.Panic", "example.com/spawnresults/helper.Single"} {
				if got := strings.Contains(string(symbols), name+".coro"); got != native {
					t.Errorf("%s native entry = %t, want %t", name, got, native)
				}
			}
			for _, name := range []string{"main.plain", "example.com/spawnresults/helper.Plain"} {
				if strings.Contains(string(symbols), name+".coro") {
					t.Errorf("plain target without an exported adapter lost fallback: %s", name)
				}
			}
			if native {
				cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", `^main\.spawn\.coro(\.func[0-9]+)?$`, exe)
				data, err := cmd.CombinedOutput()
				if err != nil || !strings.Contains(string(data), "runtime.coroSpawn(SB)") ||
					!strings.Contains(string(data), "runtime.coroSpawnFrame(SB)") || strings.Contains(string(data), "runtime.newproc(SB)") {
					t.Fatalf("missing native closure/frame spawn or retained ordinary go: %v\n%s", err, data)
				}
			}
			for _, procs := range []string{"1", "4"} {
				expect := "0"
				if native {
					expect = "1"
				}
				for _, scenario := range []struct{ argument, panicMode, want string }{
					{"", "", "spawn-results-ok\n"},
					{"1", "", "spawn-result-argument-ok\n"},
					{"", "explicit", "spawn-result-panic"},
					{"", "implicit", "invalid memory address or nil pointer dereference"},
				} {
					run := testenv.Command(t, exe)
					run.Env = append(run.Environ(), "GOMAXPROCS="+procs, "GOGC=1", "EXPECT_NATIVE="+expect,
						"ARGUMENT_PANIC="+scenario.argument, "PANIC="+scenario.panicMode)
					out, err := run.CombinedOutput()
					if scenario.panicMode == "" {
						if err != nil || string(out) != scenario.want {
							t.Fatalf("GOMAXPROCS=%s argument=%q: %v\n%s", procs, scenario.argument, err, out)
						}
					} else if err == nil || !strings.Contains(string(out), scenario.want) || strings.Contains(string(out), "parent-recovered-child") {
						t.Fatalf("GOMAXPROCS=%s child panic %s: %v\n%s", procs, scenario.panicMode, err, out)
					}
				}
			}
		})
	}
}
