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

func TestTerminalLogicalPanicCallbacks(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main

import (
	"context"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"
)

type panicValue struct {
	mode                 int
	stage, release, done chan struct{}
}

type stringValue panicValue
//go:noinline
func (v stringValue) String() string {goto body;body:v.mode-=5;return panicValue(v).Error()}
//go:noinline
func grow(n int) int {
	goto body
body:
	var pad [2048]byte
	pad[n%len(pad)] = byte(n)
	sum := int(pad[n%len(pad)])
	if n > 0 {
		sum += grow(n - 1)
	}
	runtime.KeepAlive(&pad)
	return sum
}

// The runtime calls Error before fatal printing. Its ordinary Go body can
// inspect the goroutine, grow its stack, lock a thread, block, or call Goexit.
//
//go:noinline
func (v panicValue) Error() string {
	goto body
body:
	if v.mode == 1 {
		print("GROW ", grow(512), "\n")
	}
	if v.mode == 2 {
		runtime.LockOSThread()
		pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("panic-printer", "changed")))
	}
	if v.mode == 3 {
		v.stage <- struct{}{}
		<-v.release
	}
	runtime.GC()
	print("ERROR_COUNT ", runtime.NumGoroutine(), "\n")
	b := make([]byte, 1<<20)
	n := runtime.Stack(b, false)
	print("BEGIN_ERROR\n", string(b[:n]), "END_ERROR\n")
	if v.mode == 4 {
		defer close(v.done)
		runtime.Goexit()
	}
	return "printer-probe"
}

//go:noinline
func leaf(ready, release chan struct{}, value panicValue) {
	runtime.Gosched()
	ready <- struct{}{}
	<-release
	if value.mode>=5 {panic(stringValue(value))}
	panic(value)
}

//go:noinline
func recursive(n int, ready, release chan struct{}, value panicValue) {
	runtime.Gosched()
	if n > 0 {
		recursive(n-1, ready, release, value)
		return
	}
	leaf(ready, release, value)
}

//go:noinline
func launch(ready, release chan struct{}, value panicValue) {
	go recursive(4, ready, release, value)
}

var entry = launch

func main() {
	goto body
body:
	mode, err := strconv.Atoi(os.Args[1])
	if err != nil {
		panic(err)
	}
	pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("panic-printer", "source")))
	ready, release := make(chan struct{}), make(chan struct{})
	v := panicValue{mode: mode, stage: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	entry(ready, release, v)
	<-ready
	b := make([]byte, 1<<20)
	n := runtime.Stack(b, true)
	print("BEGIN_BEFORE\n", string(b[:n]), "END_BEFORE\n")
	close(release)
	if mode%5 == 3 {
		<-v.stage
		runtime.GC()
		close(v.release)
	}
	if mode%5 == 4 {
		<-v.done
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() != 1 && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		runtime.GC()
		if runtime.NumGoroutine() != 1 {
			panic("panic printer Goexit did not finish its logical goroutine")
		}
		print("PASS printer Goexit\n")
		return
	}
	select {}
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"ordinary", "nocoro", "-l", ""},
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"lockrank", "coro,staticlockranking", "-l -d=coro=4", ""},
		{"ordinary-race", "nocoro", "-l", "-race"},
		{"coro-race", "coro", "-l -d=coro=4", "-race"},
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
			if config.experiment != "nocoro" && config.extra == "" {
				for _, name := range []string{"leaf", "recursive", "launch"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for method, methodName := range []string{"Error", "String"} {
				for mode, name := range []string{"inspect", "grow", "lock", "block", "goexit"} {
					for _, p := range []string{"1", "4"} {
						run := terminalPanicCommand(t, exe, strconv.Itoa(method*5+mode))
						run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0,tracebacklabels=1,tracebackancestors=3", "GOTRACEBACK=single")
						data, err := run.CombinedOutput()
						text := string(data)
						if mode == 4 {
							if err != nil || !strings.Contains(text, "PASS printer Goexit") {
								t.Fatalf("%s %s P%s: callback Goexit: %v\n%s", methodName, name, p, err, text)
							}
						} else {
							exit, ok := err.(*exec.ExitError)
							if !ok || exit.ExitCode() != 2 || !strings.Contains(text, "panic: printer-probe") {
								t.Fatalf("%s %s P%s: expected user panic: %v\n%s", methodName, name, p, err, text)
							}
						}
						if strings.Contains(text, "fatal error:") || strings.Contains(text, "fixed stack overflow") {
							t.Fatalf("%s %s P%s: runtime failure\n%s", methodName, name, p, text)
						}
						before := fatalCallbackSection(t, text, "BEFORE")
						inside := fatalCallbackSection(t, text, "ERROR")
						if strings.Contains(inside, ".coro.") || strings.Contains(inside, "stacklessCoro") {
							t.Fatalf("%s %s P%s: private callback frames\n%s", methodName, name, p, text)
						}
						targets := fatalIdentityStacks(before, "main.leaf(")
						active := fatalIdentityStacks(inside, "main.panicValue.Error(")
						if len(targets) != 1 || len(active) != 1 {
							t.Fatalf("%s %s P%s: missing callback/source identity\n%s", methodName, name, p, text)
						}
						label := "source"
						if mode == 2 {
							label = "changed"
						}
						if active[0].id != targets[0].id || !strings.Contains(active[0].header, `"panic-printer": `+label) || strings.Contains(active[0].header, "locked to thread") != (mode == 2) {
							t.Fatalf("%s %s P%s: callback lost logical identity, labels or thread state: before=%+v active=%+v\n%s", methodName, name, p, targets[0], active[0], text)
						}
						count := regexp.MustCompile(`(?m)^ERROR_COUNT ([0-9]+)$`).FindStringSubmatch(text)
						if len(count) != 2 || count[1] != "2" {
							t.Fatalf("%s %s P%s: callback lost live goroutine count\n%s", methodName, name, p, text)
						}
						if mode != 4 {
							_, after, ok := strings.Cut(text, "END_ERROR\n")
							if !ok {
								t.Fatal("missing Error/String snapshot")
							}
							fatal := fatalIdentityStacks(after, "main.leaf(")
							if len(fatal) != 1 || fatal[0].id != targets[0].id || !reflect.DeepEqual(fatal[0].created, targets[0].created) || fatal[0].ancestry != targets[0].ancestry || strings.Count(fatal[0].text, "\nmain.recursive(") != 5 {
								t.Fatalf("%s %s P%s: terminal transfer lost source frames or origin\n%s", methodName, name, p, text)
							}
						}
					}
				}
			}
		})
	}
}

func fatalCallbackSection(t *testing.T, text, section string) string {
	t.Helper()
	_, rest, ok := strings.Cut(text, "BEGIN_"+section+"\n")
	if !ok {
		t.Fatalf("missing %s start\n%s", section, text)
	}
	body, _, ok := strings.Cut(rest, "END_"+section+"\n")
	if !ok {
		t.Fatalf("missing %s end\n%s", section, text)
	}
	return body
}
