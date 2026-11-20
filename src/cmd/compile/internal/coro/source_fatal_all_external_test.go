// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestFatalAllLogicalResidents(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main

import (
	"context"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
)

//go:noinline
func ordinaryResident(ready, forever chan struct{}, locked bool) {
	goto body
body:
	if locked {
		runtime.LockOSThread()
	}
	ready <- struct{}{}
	<-forever
}

var ordinaryResidentEntry = ordinaryResident

//go:noinline
func residentPark(ready, forever chan struct{}) {
	ready <- struct{}{}
	<-forever
}

//go:noinline
func residentLeaf(kind int, ready, forever chan struct{}) {
	runtime.Gosched()
	switch kind {
	case 2, 3:
		ordinaryResidentEntry(ready, forever, kind == 3)
		return
	case 4:
		panic("suspended-resident-panic")
	case 5:
		runtime.Goexit()
	}
	residentPark(ready, forever)
}

//go:noinline
func residentRecursive(n, kind int, ready, forever chan struct{}) {
	runtime.Gosched()
	if n > 0 {
		residentRecursive(n-1, kind, ready, forever)
		return
	}
	residentLeaf(kind, ready, forever)
}

//go:noinline
func residentRoot(n, kind int, ready, forever chan struct{}) {
	if kind == 4 || kind == 5 {
		defer residentPark(ready, forever)
	}
	residentRecursive(n, kind, ready, forever)
}

//go:noinline
func launchResident(n, kind int, ready, forever chan struct{}) {
	go residentRoot(n, kind, ready, forever)
}

var residentEntry = residentRoot
var launchResidentEntry = launchResident

//go:noinline
func ordinaryPublicResident(n int, ready, forever chan struct{}) {
	goto body
body:
	residentEntry(n, 0, ready, forever)
}

//go:noinline
func shortResident(done chan struct{}) { runtime.Gosched(); done <- struct{}{} }

//go:noinline
func launchShort(done chan struct{}) { go shortResident(done) }

var launchShortEntry = launchShort

//go:noinline
func background(mode string, ready chan struct{}) {
	goto body
body:
	close(ready)
	for {
		switch mode {
		case "gc", "multiple":
			runtime.GC()
		case "profile":
			p := make([]runtime.StackRecord, 256)
			runtime.GoroutineProfile(p)
			runtime.Stack(make([]byte, 1<<18), true)
		case "churn":
			done := make(chan struct{})
			launchShortEntry(done)
			<-done
		default:
			runtime.Gosched()
		}
	}
}

//go:noinline
func growPrinter(n int) {
	var pad [1024]byte
	pad[0] = byte(n)
	if n > 0 {
		growPrinter(n - 1)
	} else {
		runtime.GC()
	}
	runtime.KeepAlive(&pad)
}

type fatalValue struct{}

func (fatalValue) Error() string { growPrinter(128); return "resident-fatal" }

//go:noinline
func independentCrash() { runtime.Gosched(); panic(fatalValue{}) }

//go:noinline
func launchCrash() { go independentCrash() }

var launchCrashEntry = launchCrash

//go:noinline
func ordinaryCrash(ready, release chan struct{}) {
	goto body
body:
	ready <- struct{}{}
	<-release
	panic("resident-fatal")
}

func main() {
	goto body
body:
	mode := os.Args[1]
	depth, err := strconv.Atoi(os.Args[2])
	if err != nil {
		panic(err)
	}
	ready, forever := make(chan struct{}), make(chan struct{})
	for i := 0; i < 12; i++ {
		pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("fatal-resident", strconv.Itoa(i))))
		kind := i % 6
		if kind == 1 {
			go ordinaryPublicResident(depth, ready, forever)
		} else {
			launchResidentEntry(depth, kind, ready, forever)
		}
		<-ready
	}
	pprof.SetGoroutineLabels(context.Background())
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	if n == len(buf) {
		panic("truncated resident snapshot")
	}
	// Large snapshots may exceed the subprocess pipe capacity. Unlike
	// the runtime print builtin, os.File.Write handles partial writes.
	if _, err := os.Stderr.WriteString("BEGIN_RESIDENTS\n"); err != nil {
		panic(err)
	}
	if _, err := os.Stderr.Write(buf[:n]); err != nil {
		panic(err)
	}
	if _, err := os.Stderr.WriteString("END_RESIDENTS\n"); err != nil {
		panic(err)
	}
	if mode == "cpu" {
		if err := pprof.StartCPUProfile(io.Discard); err != nil {
			panic(err)
		}
	}
	if mode != "plain" && mode != "independent" {
		started := make(chan struct{})
		go background(mode, started)
		<-started
	}
	if mode == "independent" {
		launchCrashEntry()
		select {}
	}
	if mode == "multiple" {
		ready, release := make(chan struct{}), make(chan struct{})
		for i := 0; i < 8; i++ {
			go ordinaryCrash(ready, release)
		}
		for i := 0; i < 8; i++ {
			<-ready
		}
		close(release)
		select {}
	}
	panic(fatalValue{})
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct {
		name, experiment, flags, extra string
		native                         bool
	}{
		{"ordinary", "nocoro", "-l", "", false},
		{"native", "coro", "-l -d=coro=4", "", true},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", "", true},
		{"inline", "coro", "-d=coro=4", "", true},
		{"lockrank", "coro,staticlockranking", "-l -d=coro=4", "", true},
		{"ordinary-race", "nocoro", "-l", "-race", false},
		{"coro-race", "coro", "-l -d=coro=4", "-race", false},
	} {
		t.Run(config.name, func(t *testing.T) {
			if config.extra == "-race" {
				testenv.MustHaveCGO(t)
			}
			exe := filepath.Join(dir, config.name)
			args := []string{"build", "-gcflags=" + config.flags, "-o", exe}
			if config.extra != "" {
				args = append(args, config.extra)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.native {
				for _, name := range []string{"residentPark", "residentLeaf", "residentRecursive", "residentRoot", "launchResident", "shortResident", "launchShort", "independentCrash", "launchCrash"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, mode := range []string{"plain", "independent", "gc", "profile", "churn", "cpu", "multiple"} {
				for _, p := range []string{"1", "4"} {
					for _, depth := range []string{"4", "128"} {
						label := config.name + "-" + mode + "-p" + p + "-depth" + depth
						run := terminalPanicCommand(t, exe, mode, depth)
						env := []string{"GOMAXPROCS=" + p, "GOGC=1", "GODEBUG=asyncpreemptoff=0,tracebacklabels=1,tracebackancestors=3", "GOTRACEBACK=all"}
						run.Env = append(run.Environ(), env...)
						data, err := run.CombinedOutput()
						exit, ok := err.(*exec.ExitError)
						text := string(data)
						if !ok || exit.ExitCode() != 2 || !strings.Contains(text, "panic: resident-fatal") || strings.Contains(text, "fatal error:") || strings.Contains(text, "panic during panic") {
							t.Fatalf("%s: expected ordinary fatal panic: %v\n%s", label, err, text)
						}
						_, rest, ok := strings.Cut(text, "BEGIN_RESIDENTS\n")
						if !ok {
							t.Fatalf("%s: no before snapshot\n%s", label, text)
						}
						before, after, ok := strings.Cut(rest, "END_RESIDENTS\n")
						if !ok {
							t.Fatalf("%s: no after snapshot\n%s", label, text)
						}
						wants, gots := fatalIdentityStacks(before, "main.residentRoot("), fatalIdentityStacks(after, "main.residentRoot(")
						if len(wants) != 12 || len(gots) != 12 {
							t.Fatalf("%s: resident counts %d/%d, want12/12\n%s", label, len(wants), len(gots), text)
						}
						byID := make(map[string]fatalIdentityStack)
						for _, got := range gots {
							if _, ok := byID[got.id]; ok {
								t.Fatalf("%s: duplicate resident %s\n%s", label, got.id, text)
							}
							byID[got.id] = got
						}
						labelRE := regexp.MustCompile(`"fatal-resident": ([0-9]+)`)
						seen := make(map[int]bool)
						for _, want := range wants {
							got, ok := byID[want.id]
							wantLabel := labelRE.FindStringSubmatch(want.header)
							gotLabel := labelRE.FindStringSubmatch(got.header)
							if !ok || len(wantLabel) != 2 || !reflect.DeepEqual(wantLabel, gotLabel) || !reflect.DeepEqual(want.created, got.created) || want.ancestry != got.ancestry {
								t.Fatalf("%s: resident identity mismatch: before=%+v after=%+v\n%s", label, want, got, text)
							}
							index, err := strconv.Atoi(wantLabel[1])
							if err != nil || index < 0 || index >= 12 || seen[index] {
								t.Fatalf("%s: invalid label %q", label, wantLabel)
							}
							seen[index] = true
							if strings.Contains(got.header, "locked to thread") != (index%6 == 3) {
								t.Fatalf("%s: wrong thread state: %+v", label, got)
							}
							if strings.Contains(got.text, ".coro.") || strings.Contains(got.text, "stacklessCoro") || strings.Count(got.text, "\nmain.residentRecursive(") != strings.Count(want.text, "\nmain.residentRecursive(") || strings.Contains(got.text, "frames elided") != strings.Contains(want.text, "frames elided") {
								t.Fatalf("%s: resident source chain changed\n%s", label, text)
							}
						}
					}
				}
			}
		})
	}
}
