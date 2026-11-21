// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTailFrameSpawn(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
		runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main

import (
	"runtime"
	"strings"
	"sync/atomic"
)

var argumentCount, defers, ready atomic.Uint32

func argument(v int) *int {
	argumentCount.Add(1)
	return &v
}

func worker(done chan<- int, gate <-chan struct{}, value *int) {
	runtime.Gosched()
	ready.Add(1)
	<-gate
	done <- *value
}

func tail(done chan<- int, gate <-chan struct{}, v int) {
	runtime.Gosched()
	go worker(done, gate, argument(v))
}

//go:noinline
func direct(done chan<- int, gate <-chan struct{}, v int) {
	go worker(done, gate, argument(v))
	return
}

func nativeCaller(done chan<- int, gate <-chan struct{}, v int) {
	runtime.Gosched()
	direct(done, gate, v)
}

func nested(done chan<- int, gate <-chan struct{}, v int) {
	runtime.Gosched()
	go tail(done, gate, v)
}

func after(done chan<- int, gate <-chan struct{}, v int) {
	go worker(done, gate, argument(v))
	runtime.Gosched()
}

func deferMark() { defers.Add(1) }

func deferred(done chan<- int, gate <-chan struct{}, v int) {
	defer deferMark()
	go worker(done, gate, argument(v))
}

func main() {
	goto ordinary
ordinary:
	done := make(chan int)
	gate := make(chan struct{})
	direct(done, gate, 37)
	for ready.Load() == 0 { runtime.Gosched() }
	buf := make([]byte, 64<<10)
	stacks := string(buf[:runtime.Stack(buf, true)])
	if !strings.Contains(stacks, "main.worker(") || !strings.Contains(stacks, "created by main.direct in goroutine ") {
		panic("direct spawn lost source identity\n" + stacks)
	}
	close(gate)
	if <-done != 37 { panic("wrong direct spawn") }

	const rounds = 20
	for i := range rounds {
		for _, spawn := range []func(chan<- int, <-chan struct{}, int){tail, nested, direct, nativeCaller, after, deferred} {
			done := make(chan int)
			gate := make(chan struct{})
			spawn(done, gate, i)
			runtime.GC()
			close(gate)
			if got := <-done; got != i { panic("lost spawn argument") }
		}
	}
	if argumentCount.Load() != 6*rounds+1 || defers.Load() != rounds {
		panic("wrong argument or defer count")
	}
	println("tail-frame-spawn-ok")
}
`

	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags string }{
		{"inline", "coro", "-d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(dir, config.name)
			build := testenv.Command(t, testenv.GoToolPath(t), "build",
				"-gcflags="+config.flags, "-o", exe, source)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				if linkedCoroStackUnsafe(t, exe, "main.worker.coro.stackgate") ||
					linkedCoroStackUnsafe(t, exe, "main.nativeCaller.coro.stackgate") {
					t.Fatal("direct child or native caller lost native admission")
				}
				direct := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
					"-s", "^main\\.direct$", exe)
				if out, err := direct.CombinedOutput(); err != nil ||
					!strings.Contains(string(out), "runtime.newproc(SB)") ||
					strings.Contains(string(out), "runtime.coroRun") {
					t.Fatalf("direct public entry must launch through Go: %v\n%s", err, out)
				}
				for _, test := range []struct{ name, spawn string }{
					{"tail", "coroSpawnFrameTail"}, {"nested", "coroSpawnFrameTail"},
					{"direct", "coroSpawnFrameTail"},
					{"after", "coroSpawnFrame"}, {"deferred", "coroSpawnFrame"},
				} {
					if linkedCoroStackUnsafe(t, exe, "main."+test.name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", test.name)
					}
					dump := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
						"-s", "^main\\."+test.name+"\\.coro\\.func1$", exe)
					if out, err := dump.CombinedOutput(); err != nil ||
						!strings.Contains(string(out), "runtime."+test.spawn+"(SB)") {
						t.Fatalf("%s must call %s: %v\n%s", test.name, test.spawn, err, out)
					}
				}
			}
			for _, procs := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+procs, "GOGC=1")
				if out, err := run.CombinedOutput(); err != nil || string(out) != "tail-frame-spawn-ok\n" {
					t.Fatalf("P%s: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
