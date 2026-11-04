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
	"strings"
	"testing"
)

// The shared source-chain walker is also reached from SIGPROF. Exercise that
// path while an ordinary callback owns a suspended native recursive chain;
// ordinary profiling tests alone do not establish that ownership condition.
func TestSourceProfileAcrossOrdinaryHost(t *testing.T) {
	testSourceProfileAcrossHost(t, false)
}

func TestSourceProfileAcrossSystemStack(t *testing.T) {
	testSourceProfileAcrossHost(t, true)
}

func testSourceProfileAcrossHost(t *testing.T, systemStack bool) {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	source := `package main
import ("os";"runtime";"runtime/pprof";"time")
var sink uint64
//go:noinline
func busy() {
 goto body
body:
 end:=time.Now().Add(time.Second)
 x:=uint64(1)
 for time.Now().Before(end) {
  for i:=0;i<100000;i++ {x=x*1664525+1013904223}
 }
 sink=x
}
//go:noinline
func recursive(n int,f func()) {
 runtime.Gosched()
 if n>0 {recursive(n-1,f);return}
 f()
}
var entry=recursive
func main() {
 goto body
body:
 if err:=pprof.StartCPUProfile(os.Stdout);err!=nil {panic(err)}
 entry(32,busy)
 pprof.StopCPUProfile()
}
`
	if systemStack {
		// Force SIGPROF to start on g0. The physical unwinder then crosses
		// systemstack back to the ordinary host that owns the native chain.
		// Avoid clock calls on g0: their platform libcall profile may stop at
		// systemstack even in ordinary Go. Only this fixture disables linkname
		// checks for the runtime test hook.
		source = strings.Replace(source, `"time"`, `_ "unsafe"`, 1)
		start := strings.Index(source, "//go:noinline\nfunc busy()")
		end := strings.Index(source[start:], "//go:noinline\nfunc recursive") + start
		source = source[:start] + `//go:linkname systemstack runtime.systemstack
func systemstack(func())
//go:noinline
func busy() {
 goto body
body:
 systemstack(systemBusy)
}
//go:noinline
//go:nosplit
func systemBusy() {
 goto body
body:
 x:=uint64(1)
 for i:=0;i<1<<29;i++ {x=x*1664525+1013904223}
 sink=x
}
` + source[end:]
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
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(dir, config.name)
			args := []string{"build", "-gcflags=" + config.flags, "-o", exe}
			if systemStack {
				args = append(args, "-ldflags=-checklinkname=0")
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" && linkedCoroStackUnsafe(t, exe, "main.recursive.coro.stackgate") {
				t.Fatal("profile owner lost native admission")
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0")
				var stderr bytes.Buffer
				run.Stderr = &stderr
				data, err := run.Output()
				if err != nil {
					t.Fatalf("P%s: %v\n%s", p, err, stderr.Bytes())
				}
				prof, err := profile.Parse(bytes.NewReader(data))
				if err != nil {
					t.Fatalf("P%s: parse profile: %v", p, err)
				}
				busyName := "main.busy"
				if systemStack {
					busyName = "main.systemBusy"
				}
				matched := 0
				for _, sample := range prof.Sample {
					var names []string
					busy, parents := false, 0
					for _, location := range sample.Location {
						for _, line := range location.Line {
							if line.Function == nil {
								continue
							}
							name := line.Function.Name
							names = append(names, name)
							busy = busy || name == busyName
							if name == "main.recursive" {
								parents++
							}
						}
					}
					if !busy {
						continue
					}
					matched++
					// CPU profiling has a fixed 64-PC buffer. Leave room for
					// signal/runtime frames so this tests the complete source
					// chain, rather than ordinary profile truncation.
					if parents != 33 {
						t.Fatalf("P%s: got %d recursive source frames, want 33: %v", p, parents, names)
					}
					for _, name := range names {
						if strings.Contains(name, ".coro") || strings.Contains(name, "stacklessCoro") {
							t.Fatalf("P%s: generated bridge in ordinary-host sample: %v", p, names)
						}
					}
				}
				if matched == 0 {
					t.Fatalf("P%s: no ordinary-host CPU samples", p)
				}
			}
		})
	}
}
