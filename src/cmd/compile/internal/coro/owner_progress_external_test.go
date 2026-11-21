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

func TestDetachedOwnerSharesSingleP(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
		runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}

	const program = `package main
import (
	"runtime"
	"sync/atomic"
)

var stop atomic.Bool
var launched int

//go:noinline
func wakeProducer(done chan<- struct{}) {
	runtime.Gosched()
	done <- struct{}{}
}

//go:noinline
func startWorker(done chan<- struct{}) {
	launched++
	go wakeProducer(done)
}

//go:noinline
func churn(ready chan<- struct{}, rounds chan<- int) {
	goto body
body:
	done := make(chan struct{})
	close(ready)
	n := 0
	for ; n < 8192 && !stop.Load(); n++ {
		startWorker(done)
		<-done
	}
	if launched != n { panic("incorrect launch count") }
	rounds <- n
}

func main() {
	goto body
body:
	ready := make(chan struct{})
	rounds := make(chan int)
	go churn(ready, rounds)
	<-ready
	stop.Store(true)
	println(<-rounds)
}
`

	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			program := filepath.Join(dir, mode.name)
			build := testenv.Command(t, testenv.GoToolPath(t), "build",
				"-gcflags="+mode.flags, "-o", program, source)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+mode.experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if mode.experiment == "coro" {
				for _, name := range []string{"startWorker", "wakeProducer"} {
					if linkedCoroStackUnsafe(t, program, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			// Disable asynchronous preemption and GC to exercise the
			// owner/producer handoff without unrelated scheduler activity.
			for range 6 {
				cmd := testenv.Command(t, program)
				cmd.Env = append(cmd.Environ(), "GOMAXPROCS=1", "GOGC=off", "GODEBUG=asyncpreemptoff=1")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("run: %v\n%s", err, out)
				}
				rounds, err := strconv.Atoi(strings.TrimSpace(string(out)))
				if err != nil || rounds < 0 || rounds > 64 {
					t.Fatalf("ordinary foreground delayed by %s detached tasks", out)
				}
			}
		})
	}
}
