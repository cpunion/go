// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The crashing goroutine must keep its source identity after terminal frames
// are released. This checks that goroutine only; enumeration of other idle
// native goroutines by the fatal all-goroutine printer is a separate contract.
func TestTerminalLogicalPanicIdentity(t *testing.T) {
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
)

//go:noinline
func ordinaryPanic() { goto body; body: panic("identity-probe") }
//go:noinline
func ordinaryLockedPanic() { goto body; body: runtime.LockOSThread(); panic("identity-probe") }
//go:noinline
func ordinaryRelabeledPanic() { goto body; body: pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("fatal-root", "changed"))); panic("identity-probe") }
//go:noinline
func warmRoot(){defer func(){recover()}();runtime.Gosched();panic("previous")}
var warmEntry=warmRoot
//go:noinline
func ordinaryUnrelatedPanic(ready,release chan struct{}){goto body;body:ready<-struct{}{};<-release;panic("identity-probe")}
var scenario string
//go:noinline
func nativeReplacement() { runtime.Gosched(); panic("identity-probe") }
//go:noinline
func nestedRecovery() { defer func(){recover()}(); runtime.Gosched(); panic("ignored") }
//go:noinline
func crasher(ready, release chan struct{}, crash func()) {
 runtime.Gosched()
 ready <- struct{}{}
 <-release
 switch scenario {
 case "independent-native-defer": defer nativeReplacement()
 case "independent-recovered-defer": defer nestedRecovery()
 case "independent-goexit": defer nativeReplacement(); runtime.Goexit()
 case "independent-nil-goexit": var fn func(); defer fn(); runtime.Goexit()
 }
 if crash != nil { crash(); return }
 panic("identity-probe")
}
//go:noinline
func recursive(n int, ready, release chan struct{}, crash func()) {
 runtime.Gosched()
 if n > 0 { recursive(n-1, ready, release, crash); return }
 crasher(ready, release, crash)
}
//go:noinline
func launcher(ready, release chan struct{}, crash func()) { go recursive(4, ready, release, crash) }
//go:noinline
func parked(ready, forever chan struct{}) { ready <- struct{}{}; <-forever }
//go:noinline
func launchIdle(ready, forever chan struct{}) { go parked(ready, forever) }
var entry = recursive
var launchEntry = launcher
var idleEntry = launchIdle
//go:noinline
func snapshot(ready, release chan struct{}) {
 goto body
body:
 <-ready
 b := make([]byte, 1<<20)
 n := runtime.Stack(b, true)
 print("BEGIN_BEFORE\n", string(b[:n]), "END_BEFORE\n")
 close(release)
}
func main() {
 goto body
body:
 if os.Args[1] == "independent-after-recovery" || os.Args[1] == "ordinary-after-recovery" {
  pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("fatal-root", "previous")))
  for i:=0;i<128;i++{warmEntry()}
 }
 pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("fatal-root", "source")))
 idleReady, forever := make(chan struct{}), make(chan struct{})
 idleEntry(idleReady, forever)
 <-idleReady
 ready, release := make(chan struct{}), make(chan struct{})
 var crash func()
 mode := os.Args[1]
 scenario = mode
 if mode == "ordinary-after-recovery" {go snapshot(ready,release);ordinaryUnrelatedPanic(ready,release);return}
 if mode == "public-host" || mode == "independent-host" { crash = ordinaryPanic }
 if mode == "public-locked" || mode == "independent-locked" { crash = ordinaryLockedPanic }
 if mode == "public-relabeled" || mode == "independent-relabeled" { crash = ordinaryRelabeledPanic }
 if mode == "public" || mode == "public-host" || mode == "public-locked" || mode == "public-relabeled" {
  go snapshot(ready, release)
  entry(4, ready, release, crash)
 } else {
  launchEntry(ready, release, crash)
  snapshot(ready, release)
  select{}
 }
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
		{"no-dwarf", "coro", "-l -d=coro=4", "-ldflags=-w", true},
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
				for _, name := range []string{"crasher", "recursive", "launcher", "parked", "launchIdle", "warmRoot"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, mode := range []string{"public", "public-host", "public-locked", "public-relabeled", "independent", "independent-host", "independent-locked", "independent-relabeled", "independent-native-defer", "independent-recovered-defer", "independent-goexit", "independent-nil-goexit", "independent-after-recovery", "ordinary-after-recovery"} {
				for _, p := range []string{"1", "4"} {
					for _, level := range []string{"single", "all"} {
						run := terminalPanicCommand(t, exe, mode)
						run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0,tracebacklabels=1,tracebackancestors=3", "GOTRACEBACK="+level)
						data, err := run.CombinedOutput()
						text := string(data)
						exit, ok := err.(*exec.ExitError)
						panicText := "panic: identity-probe"
						if mode == "independent-nil-goexit" {
							panicText = "panic: runtime error: invalid memory address or nil pointer dereference"
						}
						if !ok || exit.ExitCode() != 2 || !strings.Contains(text, panicText) || strings.Contains(text, "fatal error:") {
							t.Fatalf("%s P%s %s: expected uncaught user panic: %v\n%s", mode, p, level, err, text)
						}
						_, rest, ok := strings.Cut(text, "BEGIN_BEFORE\n")
						if !ok {
							t.Fatalf("missing before snapshot: %s", text)
						}
						before, after, ok := strings.Cut(rest, "END_BEFORE\n")
						if !ok {
							t.Fatalf("missing after snapshot: %s", text)
						}
						marker, recursiveFrames := "main.crasher(", 5
						if mode == "ordinary-after-recovery" {
							marker, recursiveFrames = "main.ordinaryUnrelatedPanic(", 0
						}
						targets := fatalIdentityStacks(before, marker)
						failures := fatalIdentityStacks(after, marker)
						if len(targets) != 1 || len(failures) != 1 {
							t.Fatalf("%s P%s %s: target count %d/%d\n%s", mode, p, level, len(targets), len(failures), text)
						}
						want, got := targets[0], failures[0]
						if got.id != want.id || !reflect.DeepEqual(got.created, want.created) || got.ancestry != want.ancestry {
							t.Fatalf("%s P%s %s: lost identity or origin: before=%+v after=%+v\n%s", mode, p, level, want, got, text)
						}
						label := "source"
						if strings.HasSuffix(mode, "-relabeled") {
							label = "changed"
						}
						if !strings.Contains(got.header, `"fatal-root": `+label) || strings.Contains(got.header, "locked to thread") != strings.HasSuffix(mode, "-locked") {
							t.Fatalf("%s P%s %s: wrong label or thread state: %s\n%s", mode, p, level, got.header, text)
						}
						if strings.Count(got.text, "\nmain.recursive(") != recursiveFrames || strings.Contains(got.text, ".coro.") || strings.Contains(got.text, "stacklessCoro") {
							t.Fatalf("%s P%s %s: incomplete or private source frames\n%s", mode, p, level, text)
						}
						if (strings.HasPrefix(mode, "public") || mode == "ordinary-after-recovery") && (got.id != "1" || len(got.created) != 0) {
							t.Fatalf("main has wrong identity or creator: %+v", got)
						}
						if strings.HasPrefix(mode, "independent") && (len(got.created) != 1 || got.created[0] != "created by main.launcher in goroutine 1" || got.ancestry == "") {
							t.Fatalf("independent source origin missing: %+v", got)
						}
					}
				}
			}
		})
	}
}

type fatalIdentityStack struct {
	id, header, text, ancestry string
	created                    []string
}

func fatalIdentityStacks(text, marker string) []fatalIdentityStack {
	headers := regexp.MustCompile(`(?m)^goroutine ([0-9]+) (.*):$`)
	positions := headers.FindAllStringSubmatchIndex(text, -1)
	var result []fatalIdentityStack
	for i, pos := range positions {
		end := len(text)
		if i+1 < len(positions) {
			end = positions[i+1][0]
		}
		block := text[pos[0]:end]
		if !strings.Contains(block, "\n"+marker) {
			continue
		}
		stack := fatalIdentityStack{id: text[pos[2]:pos[3]], header: text[pos[4]:pos[5]], text: block}
		if at := strings.Index(block, "[originating from goroutine "); at >= 0 {
			stack.ancestry = strings.TrimSpace(block[at:])
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "created by ") {
				stack.created = append(stack.created, line)
			}
		}
		result = append(result, stack)
	}
	return result
}

// Bound each fatal subprocess independently so an invalid host handoff cannot
// consume the package deadline before leaving a useful diagnostic.
func terminalPanicCommand(t *testing.T, exe string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return testenv.CommandContext(t, ctx, exe, args...)
}
