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

func TestNestedDeferCapture(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"

var gcRequest = make(chan bool)
var gcDone = make(chan bool)

func collect() {
	for range gcRequest { runtime.GC(); gcDone <- true }
}

type counter struct { value int }

func (c counter) result(seed int) (value int) {
	defer func() {
		value += func() int { return func() int { return c.value + seed }() }()
	}()
	seed++
	gcRequest <- true
	<-gcDone
	value = seed
	return
}

func repeated(count int) (value int) {
	i := 0
	for ; i < count; i++ {
		copy := i
		defer func() { value += func() int { return copy }() }()
	}
	gcRequest <- true
	<-gcDone
	return
}

func recovered(seed int) (value int) {
	defer func() {
		if recover() != 42 { panic("recover changed") }
		value += func() int { return seed }()
	}()
	runtime.Gosched()
	panic(42)
}

var observed int

func exiting(seed int, done chan bool) {
	defer func() {
		observed = func() int { return seed }()
		close(done)
	}()
	runtime.Gosched()
	runtime.Goexit()
}

func main() {
	for range 1 {
		go collect()
		for i := 0; i < 20; i++ {
			if got := (counter{3}).result(5); got != 15 { panic("receiver/parameter/result capture changed") }
			if got := repeated(5); got != 10 { panic("per-iteration defer capture changed") }
			if got := recovered(42); got != 42 { panic("defer recover result changed") }
			done := make(chan bool)
			go exiting(42, done)
			<-done
			if observed != 42 { panic("defer Goexit capture changed") }
		}
		close(gcRequest)
	}
	println("nested-defer-capture-ok")
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
			exe := filepath.Join(t.TempDir(), "defer-capture.exe")
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
				for _, name := range []string{"counter.result", "recovered", "exiting"} {
					if !strings.Contains(string(symbols), "main."+name+".coro") {
						t.Errorf("missing native factory %s\n%s", name, out)
					}
				}
				// Repeated nested captures still use the existing conservative
				// fallback unless inlining removes the nested closure.
				if config.name != "inline" && !strings.Contains(string(out),
					"coro: skip main.repeated: nested repeated source defer capture") {
					t.Errorf("repeated nested capture did not preserve its fallback boundary\n%s", out)
				}
			}
			run := testenv.Command(t, exe)
			run.Env = append(run.Environ(), "GOMAXPROCS=1", "GOGC=1")
			if out, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "nested-defer-capture-ok" {
				t.Errorf("execution = (%v, %q), want nested-defer-capture-ok", err, out)
			}
		})
	}
}
