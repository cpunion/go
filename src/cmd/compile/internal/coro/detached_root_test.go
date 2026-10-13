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
	"strings"
	"testing"
	"time"
)

func TestDetachedRoot(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	testenv.MustHaveCGO(t)
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
			t.Skip("direct C calls require a supported native executor")
		}
	}
	tmp := t.TempDir()
	const source = `package main

/*
#cgo nocallback blocked
#include <stdint.h>
void blocked(uint32_t *gate) {
	while (__atomic_load_n(gate, __ATOMIC_ACQUIRE) == 0) {}
}
*/
import "C"

import (
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
)

var gate, entered uint32
var done = make(chan struct{}, 1)

func blockingChild() {
	atomic.StoreUint32(&entered, 1)
	C.blocked((*C.uint32_t)(unsafe.Pointer(&gate)))
	done <- struct{}{}
}

func spawnBlocking() {
	go blockingChild()
	for atomic.LoadUint32(&entered) == 0 {
		runtime.Gosched()
	}
}

func timerChild() {
	time.Sleep(time.Millisecond)
	done <- struct{}{}
}

func spawnTimer() {
	go timerChild()
	runtime.Gosched()
}

func main() {
	runtime.GOMAXPROCS(1)
	// The range keeps the controller on an ordinary Go stack so each call
	// below owns a public coroutine root, rather than an await frame.
	for range 1 {
		spawnBlocking()
		atomic.StoreUint32(&gate, 1)
		<-done
		spawnTimer()
		<-done
	}
	println("detached children complete")
}
`
	path := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(path, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, experiment := range []string{"coro", "nocoro"} {
		t.Run(experiment, func(t *testing.T) {
			exe := filepath.Join(tmp, experiment+".exe")
			flags := "-l"
			if experiment == "coro" {
				flags += " -d=coro=4"
			}
			build := testenv.Command(t, testenv.GoToolPath(t), "build",
				"-gcflags="+flags, "-o", exe, path)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			data, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if experiment == "coro" {
				for _, name := range []string{"blockingChild", "spawnBlocking", "timerChild", "spawnTimer"} {
					if !strings.Contains(string(data), "func="+name+".coro,") {
						t.Errorf("%s was not lowered\n%s", name, data)
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			run := testenv.CommandContext(t, ctx, exe)
			data, err = run.CombinedOutput()
			if err != nil || string(data) != "detached children complete\n" {
				t.Errorf("run: %v\n%s", err, data)
			}
		})
	}
}
