// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMutex(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	const source = `package main

import (
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var mu sync.Mutex
var count int
var inside, overlapped, workerID int32
var completed [8]int
var done = make(chan int, 8)

func uncontended() {
	for i := 0; i < 20; i++ {
		mu.Lock()
		count++
		mu.Unlock()
	}
	if count != 20 { panic("uncontended count") }
}

func deferredUnlock() {
	mu.Lock()
	defer mu.Unlock()
	count++
}

func mutexWorker() {
	id := int(atomic.AddInt32(&workerID, 1)) - 1
	for i := 0; i < 40; i++ {
		mu.Lock()
		if atomic.AddInt32(&inside, 1) != 1 { atomic.StoreInt32(&overlapped, 1) }
		count++
		completed[id]++
		if i == 0 {
			// Force contention beyond Mutex's starvation threshold.
			time.Sleep(2 * time.Millisecond)
		} else {
			runtime.Gosched()
		}
		if atomic.AddInt32(&inside, -1) != 0 { atomic.StoreInt32(&overlapped, 1) }
		mu.Unlock()
	}
	done <- id
}

func contention() {
	count = 0
	for id := 0; id < 8; id++ { go mutexWorker() }
	for id := 0; id < 8; id++ { <-done }
	if count != 320 { panic("contended count") }
	if atomic.LoadInt32(&overlapped) != 0 { panic("overlapping mutex owners") }
	for id := 0; id < 8; id++ {
		if completed[id] != 40 { panic("mutex waiter starved") }
	}
}

func detachedWaiter() {
	mu.Lock()
	count++
	mu.Unlock()
	done <- 1
}

func spawnDetached() {
	go detachedWaiter()
	// This white-box check uses the repository's Mutex layout and waiter
	// count to prove the child has reached Lock before its root returns.
	for atomic.LoadInt32((*int32)(unsafe.Pointer(&mu))) >> 3 == 0 {
		runtime.Gosched()
	}
}

func main() {
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
	// Keep the public-entry controller on an ordinary Go stack. The functions
	// called below must still use generated coroutine factories and resumes.
	for range 1 {
		uncontended()
		deferredUnlock()
		if count != 21 { panic("deferred unlock fallback") }
		contention()
		mu.Lock()
		spawnDetached()
		if count != 320 { panic("detached child acquired a locked mutex") }
		mu.Unlock()
		<-done
		if count != 321 { panic("detached child lost after root return") }
	}
	if n, _ := runtime.BlockProfile(nil); n == 0 { panic("missing block profile") }
	if n, _ := runtime.MutexProfile(nil); n == 0 { panic("missing mutex profile") }
	println("mutex complete")
}
`
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"),
		[]byte("module example.com/coromutex\n\ngo 1.28\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct {
		name, experiment, flags string
	}{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4"},
		{"disabled-inline", "nocoro", ""},
		{"disabled-noinline", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(tmp, config.name+".exe")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe,
				"-gcflags=internal/sync="+config.flags,
				"-gcflags=sync="+config.flags,
				"-gcflags=example.com/coromutex="+config.flags, ".")
			build.Dir = tmp
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			data, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" {
				if strings.Contains(string(data), "func=deferredUnlock.coro,") {
					t.Errorf("suspending deferred unlock was lowered\n%s", data)
				}
				for _, name := range []string{
					"uncontended", "mutexWorker", "contention", "detachedWaiter", "spawnDetached",
				} {
					if !strings.Contains(string(data), "func="+name+".coro,") {
						t.Errorf("%s was not lowered\n%s", name, data)
					}
				}
				nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
				symbols, err := nm.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, symbols)
				}
				for _, name := range []string{
					"internal/sync.(*Mutex).Lock", "internal/sync.(*Mutex).lockSlow",
					"internal/sync.(*Mutex).Unlock", "internal/sync.(*Mutex).unlockSlow",
					"sync.(*Mutex).Lock", "sync.(*Mutex).Unlock",
				} {
					if !strings.Contains(string(symbols), name+".coro") {
						t.Errorf("missing generated entry for %s", name)
					}
				}
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				run := testenv.CommandContext(t, ctx, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs))
				data, err := run.CombinedOutput()
				cancel()
				if err != nil || string(data) != "mutex complete\n" {
					t.Errorf("P%d: %v\n%s", procs, err, data)
				}
			}
		})
	}
}
