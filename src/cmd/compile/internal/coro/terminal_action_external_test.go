// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStableTerminalActionSource(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main

import "runtime"

var pointer *int

func leaf(mode int) int {
	runtime.Gosched()
	if mode == 2 { return *pointer }
	return 42
}

func first(depth, mode int) int {
	if depth == 0 {
		value := leaf(mode)
		return value
	}
	value := second(depth-1, mode)
	return value+1
}

func second(depth, mode int) int {
	if depth == 0 {
		value := leaf(mode)
		return value
	}
	value := first(depth-1, mode)
	return value+1
}

func terminalLeaf(mode int) int {
	runtime.Gosched()
	if mode == 1 { panic(42) }
	if mode == 3 { runtime.Goexit() }
	return 42
}

func third(depth, mode int) int {
	if depth == 0 {
		value := terminalLeaf(mode)
		return value
	}
	value := fourth(depth-1, mode)
	return value+1
}

func fourth(depth, mode int) int {
	if depth == 0 {
		value := terminalLeaf(mode)
		return value
	}
	value := third(depth-1, mode)
	return value+1
}

func recovered(mode int) (value any) {
	defer func() { value = recover() }()
	if mode == 2 { first(129, mode) } else { third(129, mode) }
	return nil
}

func exiting(done chan bool) {
	defer close(done)
	third(129, 3)
	panic("Goexit returned")
}

func main() {
	for range 10 {
		if first(4096, 0) != 4138 { panic("normal result changed") }
		if recovered(1) != 42 { panic("child panic was lost") }
		if _, ok := recovered(2).(runtime.Error); !ok { panic("implicit panic was lost") }
		done := make(chan bool)
		go exiting(done)
		<-done
		runtime.GC()
		if first(64, 0) != 106 { panic("post-terminal reuse changed") }
	}
	println("stable-terminal-action-ok")
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
		{"race", "coro", "-l -d=coro=4"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "terminal.exe")
			args := []string{"build"}
			if config.name == "race" {
				testenv.MustHaveCGO(t)
				args = append(args, "-race")
			}
			args = append(args, "-gcflags="+config.flags, "-o", exe, source)
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			symbols, err := nm.CombinedOutput()
			if err != nil {
				t.Fatalf("nm: %v\n%s", err, symbols)
			}
			native := config.experiment == "coro" && config.name != "race"
			for _, name := range []string{"first", "second", "leaf", "third", "fourth", "terminalLeaf"} {
				if got := strings.Contains(string(symbols), "main."+name+".coro"); got != native {
					t.Errorf("%s native factory = %v, want %v\n%s", name, got, native, out)
				}
				if !native {
					continue
				}
				dump := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", "main\\."+name+"\\.coro\\.func1$", exe)
				out, err := dump.CombinedOutput()
				want := 1
				if name == "third" || name == "fourth" || name == "terminalLeaf" {
					want = 2 // Known terminal effects retain the conservative path.
				}
				if got := strings.Count(string(out), "CALL runtime.coroTerminalAction"); err != nil || got != want {
					t.Errorf("%s: %d terminal queries, want %d: %v\n%s", name, got, want, err, out)
				}
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				run := testenv.CommandContext(t, ctx, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs), "GOGC=1")
				out, err := run.CombinedOutput()
				cancel()
				if err != nil || string(out) != "stable-terminal-action-ok\n" {
					t.Errorf("P%d: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
