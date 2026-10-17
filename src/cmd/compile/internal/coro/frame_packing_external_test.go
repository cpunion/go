// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestFusedFrameStatePacking(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"

var remaining, completed int

func first() {
	if remaining == 0 { runtime.Gosched(); return }
	remaining--
	second()
	completed++
}

func second() {
	if remaining == 0 { runtime.Gosched(); return }
	remaining--
	first()
	completed++
}

var ready = make(chan bool)
var release chan bool

func even(depth int, value *int) int {
	if depth == 0 {
		ready <- true
		<-release
		return *value
	}
	got := odd(depth-1, value)
	return got + 1
}

func odd(depth int, value *int) int {
	if depth == 0 {
		ready <- true
		<-release
		return *value
	}
	got := even(depth-1, value)
	return got + 1
}

func mixed(depth int, value *int) int {
	var pointers [4]*int
	pointers[0], pointers[3] = value, value
	got := even(depth, value)
	return got + *pointers[0] + *pointers[3]
}

func main() {
	for range 1 {
		for i := 0; i < 8; i++ {
			remaining, completed = 4096, 0
			first()
			if remaining != 0 || completed != 4096 { panic("packed state changed") }
			value := i + 17
			result := make(chan int)
			release = make(chan bool)
			go func() { result <- mixed(1024, &value) }()
			<-ready
			runtime.GC()
			close(release)
			if got := <-result; got != 1024+3*value { panic("suspended pointer frame changed") }
		}
	}
	println("frame-packing-ok")
}
`
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags string }{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"disabled", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "frame-packing.exe")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, source)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
				symbols, err := nm.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, symbols)
				}
				for _, name := range []string{"first", "second", "even", "odd", "mixed"} {
					if !strings.Contains(string(symbols), "main."+name+".coro") {
						t.Errorf("missing native factory %s\n%s", name, out)
					}
				}
			}
			for _, procs := range []int{1, 4} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs), "GOGC=1")
				if out, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "frame-packing-ok" {
					t.Errorf("P%d execution = (%v, %q), want frame-packing-ok", procs, err, out)
				}
			}
		})
	}
}
