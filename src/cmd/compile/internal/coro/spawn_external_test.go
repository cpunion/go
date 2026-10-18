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

func TestSpawnContextFactory(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
		runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is unavailable")
	}
	files := map[string]string{
		"go.mod": "module example.com/spawncontext\n\ngo 1.28\n",
		"helper/helper.go": `package helper

type Box struct { Value int }

func worker(value int, p *Box, ready chan<- int, release <-chan struct{}, done chan<- int) {
 ready <- value
 <-release
 done <- value + p.Value
}

func Spawn(start, n int, p *Box, ready chan<- int, release <-chan struct{}, done chan<- int) {
 for i := range n { go worker(start+i, p, ready, release, done) }
}

type Spawner struct { Base int }

func (s *Spawner) Spawn(n int, p *Box, ready chan<- int, release <-chan struct{}, done chan<- int) {
 Spawn(s.Base, n, p, ready, release, done)
}

func Groups(p *Box, ready chan<- int, release <-chan struct{}, done chan<- int) int {
 s := Spawner{Base: 64}
 Spawn(0, 64, p, ready, release, done)
 s.Spawn(64, p, ready, release, done)
 return 128
}
`,
		"main.go": `package main

import (
 "example.com/spawncontext/helper"
 "os"
 "runtime"
)

func relay(p *helper.Box, ready chan<- int, release <-chan struct{}, done chan<- int) int {
 return helper.Groups(p, ready, release, done)
}

func launch(ready chan<- int, release <-chan struct{}, done chan<- int, created chan<- int) {
 p := &helper.Box{Value: 17}
 n := relay(p, ready, release, done)
 created <- n
}

func main() {
 ready, done := make(chan int, 128), make(chan int, 128)
 release, created, finished := make(chan struct{}), make(chan int), make(chan struct{})
 baseline := runtime.NumGoroutine()
 go func() { launch(ready, release, done, created); close(finished) }()
 // The creator returns while every child is still waiting for release.
 if <-created != 128 { panic("creator did not return") }
 <-finished
 var seen [128]bool
 for range 128 {
  value := <-ready
  if value < 0 || value >= 128 || seen[value] { panic("spawn argument snapshot") }
  seen[value] = true
 }
 if os.Getenv("EXPECT_NATIVE") == "1" && runtime.NumGoroutine() > baseline+12 {
  panic("pure spawn helper created ordinary goroutines")
 }
 runtime.GC()
 close(release)
 sum := 0
 for range 128 { sum += <-done }
 if sum != 128*127/2+128*17 { panic("parked pointer root") }
 println("spawn-context-ok")
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
			tmp := t.TempDir()
			for name, contents := range files {
				path := filepath.Join(tmp, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(contents), 0o666); err != nil {
					t.Fatal(err)
				}
			}
			exe := filepath.Join(tmp, "spawn")
			flags, experiment := "-d=coro=4", "coro"
			if mode != "inline" {
				flags += " -l"
			}
			if mode == "nocoro" {
				flags, experiment = "-l", "nocoro"
			}
			args := []string{"build", "-o", exe, "-gcflags=example.com/spawncontext/...=" + flags}
			if mode == "checkptr" {
				args = append(args, "-gcflags=all=-d=checkptr=2",
					"-gcflags=example.com/spawncontext/...="+flags+" -d=checkptr=2")
			}
			if mode == "missing-factory" {
				args = append(args, "-gcflags=example.com/spawncontext/helper=-l -d=coro=1")
			}
			if mode == "race" {
				args = append(args, "-race")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, ".")...)
			build.Dir = tmp
			build.Env = append(build.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if mode == "missing-factory" && !strings.Contains(string(out), "unsupported coroutine dependency") {
				t.Fatalf("missing imported factory did not force fallback:\n%s", out)
			}
			native := mode == "inline" || mode == "noinline" || mode == "checkptr"
			spawners := []string{"example.com/spawncontext/helper.Spawn", "example.com/spawncontext/helper.(*Spawner).Spawn", "example.com/spawncontext/helper.Groups", "main.relay"}
			if native {
				for _, name := range spawners {
					found := false
					for _, line := range strings.Split(string(out), "\n") {
						if strings.Contains(line, "coro: func="+name+" ") &&
							strings.Contains(line, "effect=nosuspend ") &&
							strings.Contains(line, "primary=coro exec=task-context ") {
							found = true
						}
					}
					if !found {
						t.Errorf("missing nonsuspending task context for %s:\n%s", name, out)
					}
				}
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, symbols)
			}
			for _, name := range append(spawners, "main.launch", "example.com/spawncontext/helper.worker") {
				if got := strings.Contains(string(symbols), name+".coro"); got != native {
					t.Errorf("%s native entry = %t, want %t", name, got, native)
				}
			}
			if native {
				cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s",
					`^example.com/spawncontext/helper\.Spawn\.coro(\.func[0-9]+)?$`, exe)
				data, err := cmd.CombinedOutput()
				if err != nil || !strings.Contains(string(data), "runtime.coroSpawnFrame(SB)") ||
					strings.Contains(string(data), "runtime.newproc(SB)") {
					t.Fatalf("native helper retained ordinary spawn: %v\n%s", err, data)
				}
			}
			for _, procs := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				expect := "0"
				if native {
					expect = "1"
				}
				run.Env = append(run.Environ(), "GOMAXPROCS="+procs, "GOGC=1", "EXPECT_NATIVE="+expect)
				out, err := run.CombinedOutput()
				if err != nil || string(out) != "spawn-context-ok\n" {
					t.Fatalf("GOMAXPROCS=%s: run: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
