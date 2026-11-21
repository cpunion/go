// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"bytes"
	"internal/profile"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Unlike the ordinary-host and systemstack fixtures, busy remains a native
// resume here. Its recursive parents are heap activations, and public callers
// are suspended on a different physical G while the signal samples busy.
func TestSourceNativeCPUProfile(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	source := `package main
import ("context";"os";"runtime";"runtime/pprof";"strconv")
var sink uint64
var rounds=512
var promoteEntry func()
//go:noinline
func busy() {
 x:=uint64(1)
 for outer:=0;outer<rounds;outer++ {
  for i:=0;i<1000000;i++ {x=x*1664525+1013904223}
  runtime.Gosched()
 }
 sink=x
}
//go:noinline
func self(n int) {
 if n==32 {runtime.Gosched()}
 if n>0 {self(n-1);return}
 x:=uint64(1)
 for outer:=0;outer<rounds;outer++ {for i:=0;i<1000000;i++ {x=x*1664525+1013904223}} // SELF_BUSY
 sink=x
}
//go:noinline
func promote() {
 goto body
body:
 pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(),pprof.Labels("cpu-root","promoted")))
}
//go:noinline
func recursive(n int, promoted bool) {
 runtime.Gosched()
 if n>0 {recursive(n-1,promoted);return}
 if promoted {promoteEntry()}
 busy()
}
//go:noinline
func task(done chan struct{}) {recursive(32,false);done<-struct{}{}}
//go:noinline
func spawn(done chan struct{}) {go task(done)}
//go:noinline
func makeEntry(n int) func() {return func(){recursive(n,false)}}
//go:noinline
func terminalRecursive(n int, exiting bool) {
 runtime.Gosched()
 if n>0 {terminalRecursive(n-1,exiting);return}
 if exiting {runtime.Goexit()}
 panic("source-profile")
}
//go:noinline
func terminal(exiting bool, done chan struct{}) {
 defer close(done)
 defer func(){recover()}()
 defer busy()
 terminalRecursive(32,exiting)
}
//go:noinline
func recovered(done chan struct{}) {
 defer close(done)
 defer func(){recover();busy()}()
 terminalRecursive(32,false)
}
var recoveredEntry=recovered
var selfEntry=self
var entry=recursive
var spawnEntry=spawn
var terminalEntry=terminal
func main() {
 promoteEntry=promote
 if text:=os.Getenv("CORO_PROFILE_ROUNDS");text!="" {var err error;rounds,err=strconv.Atoi(text);if err!=nil||rounds<1 {panic("rounds")}}
 goto body
body:
 pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(),pprof.Labels("cpu-root","source")))
 if err:=pprof.StartCPUProfile(os.Stdout);err!=nil {panic(err)}
 switch os.Args[1] {
 case "public": entry(32,false) // PUBLIC_CALL
 case "self": selfEntry(32)
 case "promoted": entry(32,true)
 case "closure": makeEntry(32)()
 case "independent": done:=make(chan struct{});spawnEntry(done);<-done
 case "recovered": done:=make(chan struct{});go recoveredEntry(done);<-done
 case "public-panic": done:=make(chan struct{});terminalEntry(false,done);<-done
 case "panic", "goexit": done:=make(chan struct{});go terminalEntry(os.Args[1]=="goexit",done);<-done
 default: panic("mode")
 }
 pprof.StopCPUProfile()
}
`
	selfLine, publicLine := 0, 0
	for i, line := range strings.Split(source, "\n") {
		if strings.Contains(line, "SELF_BUSY") {
			selfLine = i + 1
		}
		if strings.Contains(line, "PUBLIC_CALL") {
			publicLine = i + 1
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"inline", "coro", "-d=coro=4"},
		{"lockrank", "coro,staticlockranking", "-l -d=coro=4"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(dir, config.name)
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, file)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment != "nocoro" {
				for _, name := range []string{"busy", "self", "recursive", "task", "spawn", "makeEntry.func1", "terminal", "terminalRecursive", "recovered", "recovered.func1"} {
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, mode := range []string{"public", "self", "promoted", "closure", "independent", "panic", "goexit", "recovered", "public-panic"} {
				for _, p := range []string{"1", "4"} {
					// As in runtime/pprof, increase work only when no business
					// samples arrived. Every observed semantic mismatch is fatal.
					for rounds := 512; ; rounds *= 2 {
						run := testenv.Command(t, exe, mode)
						run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0", "CORO_PROFILE_ROUNDS="+strconv.Itoa(rounds))
						var stderr bytes.Buffer
						run.Stderr = &stderr
						data, err := run.Output()
						if err != nil {
							t.Fatalf("%s/P%s: %v\n%s", mode, p, err, stderr.Bytes())
						}
						prof, err := profile.Parse(bytes.NewReader(data))
						if err != nil {
							t.Fatalf("%s/P%s: parse: %v", mode, p, err)
						}
						matched := 0
						for _, sample := range prof.Sample {
							var names []string
							busy, parents, mains := false, 0, 0
							panics, exits, callerLine := 0, 0, 0
							for _, location := range sample.Location {
								for _, line := range location.Line {
									if line.Function == nil {
										continue
									}
									name := line.Function.Name
									names = append(names, name)
									busy = busy || strings.HasPrefix(name, "main.busy") ||
										name == "main.self" && int(line.Line) == selfLine
									if name == "main.recursive" || name == "main.terminalRecursive" || name == "main.self" {
										parents++
									}
									if name == "runtime.gopanic" {
										panics++
									}
									if name == "runtime.Goexit" {
										exits++
									}
									if name == "main.main" {
										mains++
										callerLine = int(line.Line)
									}
								}
							}
							if !busy {
								continue
							}
							matched++
							wantMain := 1
							if mode == "independent" || mode == "panic" || mode == "goexit" || mode == "recovered" {
								wantMain = 0
							}
							if parents != 33 || mains != wantMain {
								t.Fatalf("%s/P%s: recursive=%d main=%d want 33/%d: %v", mode, p, parents, mains, wantMain, names)
							}
							if mode == "public" && callerLine != publicLine {
								t.Fatalf("%s/P%s: caller line=%d want %d: %v", mode, p, callerLine, publicLine, names)
							}
							wantPanics, wantExits := 0, 0
							if mode == "panic" || mode == "recovered" || mode == "public-panic" {
								wantPanics = 1
							}
							if mode == "goexit" {
								wantExits = 1
							}
							if panics != wantPanics || exits != wantExits {
								t.Fatalf("%s/P%s: panic/Goexit boundaries=%d/%d want %d/%d: %v", mode, p, panics, exits, wantPanics, wantExits, names)
							}
							for _, name := range names {
								if strings.Contains(name, ".coro") || strings.Contains(name, "stacklessCoro") {
									t.Fatalf("%s/P%s: private function in source sample: %v", mode, p, names)
								}
							}
							wantLabel := "source"
							if mode == "promoted" {
								wantLabel = "promoted"
							}
							labels := sample.Label["cpu-root"]
							if len(labels) != 1 || labels[0] != wantLabel {
								t.Fatalf("%s/P%s: labels=%v want %q", mode, p, labels, wantLabel)
							}
						}
						if matched != 0 {
							t.Logf("%s/P%s rounds=%d: %d busy sample records retain 33 recursive activations, caller and labels", mode, p, rounds, matched)
							break
						}
						// Preserve the exact compressed profile in the test log,
						// including on a later successful sampling attempt.
						t.Logf("%s/P%s rounds=%d: no busy samples; profile=%x", mode, p, rounds, data)
						if rounds == 8192 {
							t.Fatalf("%s/P%s: no busy source samples after bounded work increase", mode, p)
						}
					}
				}
			}
		})
	}
}
