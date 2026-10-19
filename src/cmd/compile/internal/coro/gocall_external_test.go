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

func TestGoCallArguments(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import (
	"os"
	"runtime"
	"sync/atomic"
)
const count = 300
type box struct { value int }
var gate = make(chan bool)
var finished = make(chan bool)
var ready, done, release uint64
var next int
var results [count]int

func argument() int { next++; return next }

func worker(index int, pointer *int, value box) {
	atomic.AddUint64(&ready, 1)
	<-gate
	runtime.Gosched()
	results[index] = *pointer + value.value
	atomic.AddUint64(&done, 1)
}

func spawn(n int) {
	pointer := new(int)
	*pointer = 17
	for i := 0; i < n; i++ {
		go worker(i, pointer, box{argument()})
	}
	for atomic.LoadUint64(&ready) < uint64(n) { runtime.Gosched() }
	for atomic.LoadUint64(&release) == 0 { runtime.Gosched() }
	close(gate)
	for atomic.LoadUint64(&done) < uint64(n) { runtime.Gosched() }
}

func launch() { spawn(count); close(finished) }

func main() {
	before := runtime.NumGoroutine()
	go launch()
	for atomic.LoadUint64(&ready) < count { runtime.Gosched() }
	runtime.GC()
	if os.Getenv("EXPECT_NATIVE") == "1" && runtime.NumGoroutine() > before+16 {
		panic("go argument wrapper retained ordinary goroutines")
	}
	atomic.StoreUint64(&release, 1)
	<-finished
	if next != count { panic("argument evaluation count") }
	for i, result := range results {
		if result != 18+i { panic("argument snapshot or pointer root") }
	}
	println("go-call-arguments-ok")
}
`
	source := filepath.Join(t.TempDir(), "main.go")
	for _, test := range []struct {
		name, experiment, flags string
		yieldArgument           bool
	}{
		{"coro/inline", "coro", "-d=coro=4", false},
		{"coro/noinline", "coro", "-l -d=coro=4", false},
		{"coro/checkptr", "coro", "-l -d=coro=4,checkptr=2", false},
		{"coro/yield-argument", "coro", "-l -d=coro=4", true},
		{"nocoro", "nocoro", "-l", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.yieldArgument {
				yieldProgram := strings.Replace(program,
					"func argument() int { next++; return next }",
					"func argument() int { runtime.Gosched(); next++; return next }", 1)
				if err := os.WriteFile(source, []byte(yieldProgram), 0o666); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
				t.Fatal(err)
			}
			exe := filepath.Join(t.TempDir(), "go-call.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
				"-gcflags="+test.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			if test.experiment == "coro" &&
				strings.Contains(string(out), "coro: skip main.spawn:") {
				t.Fatalf("spawn fell back to ordinary Go:\n%s", out)
			}
			cmd = testenv.Command(t, exe)
			expect := "0"
			if test.experiment == "coro" {
				expect = "1"
			}
			cmd.Env = append(cmd.Environ(), "GOMAXPROCS=1", "GOGC=1", "EXPECT_NATIVE="+expect)
			if out, err := cmd.CombinedOutput(); err != nil ||
				strings.TrimSpace(string(out)) != "go-call-arguments-ok" {
				t.Fatalf("execution = (%v, %q), want go-call-arguments-ok", err, out)
			}
			if test.experiment != "coro" {
				return
			}
			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
				"-s", `^main\.spawn\.coro(\.func[0-9]+)?$`, exe)
			out, err = cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "runtime.coroSpawnFrame(SB)") ||
				strings.Contains(string(out), "runtime.newproc(SB)") {
				t.Fatalf("missing native frame spawn or retained ordinary go statement: %v\n%s", err, out)
			}
		})
	}
}

func TestGoCallArgumentsFallback(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"
var done = make(chan int)
var dynamic func(int)
var next int
func argument() int { next++; return next }
func worker(value int) { runtime.Gosched(); done <- value }
func spawn() { GO_CALL }
func main() {
	dynamic = worker
	spawn()
	if <-done != 1 || next != 1 { panic("ordinary fallback argument evaluation") }
	println("go-call-fallback-ok")
}
`
	for _, test := range []struct{ name, call string }{
		{"dynamic", "go dynamic(argument())"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			source, exe := filepath.Join(dir, "main.go"), filepath.Join(dir, "fallback.exe")
			if err := os.WriteFile(source, []byte(strings.Replace(program, "GO_CALL", test.call, 1)), 0o666); err != nil {
				t.Fatal(err)
			}
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
				"-gcflags=-l -d=coro=4", "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "coro: skip main.spawn:") {
				t.Fatalf("missing conservative fallback: %v\n%s", err, out)
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOMAXPROCS=1", "GOGC=1")
			if out, err := cmd.CombinedOutput(); err != nil ||
				strings.TrimSpace(string(out)) != "go-call-fallback-ok" {
				t.Fatalf("execution = (%v, %q), want go-call-fallback-ok", err, out)
			}
			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", `^main\.spawn$`, exe)
			if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "runtime.newproc(SB)") {
				t.Fatalf("ordinary go call was not retained: %v\n%s", err, out)
			}
		})
	}
}

func TestGoCallArgumentPanic(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"
var started bool
func argument() int { panic(42) }
func worker(value int) { started = true; runtime.Gosched() }
func spawn() { go worker(argument()) }
func check() {
	defer func() {
		if recover() != 42 || started { panic("argument panic escaped its caller or spawned a child") }
	}()
	spawn()
}
func main() { check(); println("go-call-argument-panic-ok") }
`
	dir := t.TempDir()
	source, exe := filepath.Join(dir, "main.go"), filepath.Join(dir, "panic.exe")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
		"-gcflags=-l -d=coro=4,checkptr=2", "-o", exe, source)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "coro: skip main.spawn:") {
		t.Fatalf("missing native argument panic handling: %v\n%s", err, out)
	}
	cmd = testenv.Command(t, exe)
	cmd.Env = append(cmd.Environ(), "GOMAXPROCS=1", "GOGC=1")
	if out, err := cmd.CombinedOutput(); err != nil ||
		strings.TrimSpace(string(out)) != "go-call-argument-panic-ok" {
		t.Fatalf("execution = (%v, %q), want go-call-argument-panic-ok", err, out)
	}
	cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
		"-s", `^main\.spawn\.coro(\.func[0-9]+)?$`, exe)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "runtime.coroSpawnFrame(SB)") {
		t.Fatalf("missing native frame spawn: %v\n%s", err, out)
	}
}
