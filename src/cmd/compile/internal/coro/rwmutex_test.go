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

func TestRWMutex(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	const source = `package main

import (
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

var rw sync.RWMutex
var value int
var readers, writer, overlapped, shared, stage int32
var done = make(chan struct{}, 300)

func readWorker() {
	for i := 0; i < 4; i++ {
		rw.RLock()
		if atomic.AddInt32(&readers, 1) > 1 { atomic.StoreInt32(&shared, 1) }
		if atomic.LoadInt32(&writer) != 0 { atomic.StoreInt32(&overlapped, 1) }
		snapshot := value
		runtime.Gosched()
		if value != snapshot { atomic.StoreInt32(&overlapped, 1) }
		atomic.AddInt32(&readers, -1)
		rw.RUnlock()
	}
	done <- struct{}{}
}

func writeWorker() {
	for i := 0; i < 4; i++ {
		rw.Lock()
		if atomic.AddInt32(&writer, 1) != 1 || atomic.LoadInt32(&readers) != 0 {
			atomic.StoreInt32(&overlapped, 1)
		}
		value++
		runtime.Gosched()
		atomic.AddInt32(&writer, -1)
		rw.Unlock()
	}
	done <- struct{}{}
}

func parallelReaders() {
	for i := 0; i < 64; i++ { go readWorker() }
	for i := 0; i < 64; i++ { <-done }
}

func mixed() {
	for i := 0; i < 32; i++ { go readWorker() }
	for i := 0; i < 4; i++ { go writeWorker() }
	for i := 0; i < 36; i++ { <-done }
}

// This white-box layout observes the original library's writer admission
// counter; it does not modify a lock or substitute a semaphore implementation.
type rwState struct {
	w sync.Mutex
	writerSem, readerSem uint32
	readerCount, readerWait atomic.Int32
}

func pendingWriter() bool {
	return (*rwState)(unsafe.Pointer(&rw)).readerCount.Load() < 0
}

func heldWriter() {
	rw.Lock()
	atomic.StoreInt32(&stage, 1)
	rw.Unlock()
	done <- struct{}{}
}

func lateReader() {
	rw.RLock()
	if atomic.LoadInt32(&stage) != 1 { atomic.StoreInt32(&overlapped, 1) }
	rw.RUnlock()
	done <- struct{}{}
}

func spawnWriter() {
	go heldWriter()
	for !pendingWriter() { runtime.Gosched() }
}

func spawnReaders() {
	for i := 0; i < 256; i++ { go lateReader() }
	// Every reader must have entered RLock, although its queue insertion may
	// still be in progress. The ordinary controller retains its read lock.
	for (*rwState)(unsafe.Pointer(&rw)).readerCount.Load() != -(1<<30)+257 {
		runtime.Gosched()
	}
}

func main() {
	native := os.Args[1] == "native"
	if unsafe.Sizeof(rw) != unsafe.Sizeof(rwState{}) { panic("RWMutex layout changed") }
	runtime.SetBlockProfileRate(1)
	runtime.SetMutexProfileFraction(1)
	// Keep this public-entry controller on an ordinary Go stack, as in the
	// Mutex test. The worker methods still require native factories below.
	for range 1 {
		parallelReaders()
		if atomic.LoadInt32(&shared) != 1 { panic("readers were serialized") }
		mixed()
		if value != 16 { panic("writer count") }
		if atomic.LoadInt32(&overlapped) != 0 { panic("overlapping lock owners") }
		rw.RLock()
		spawnWriter()
		spawnReaders()
		if atomic.LoadInt32(&stage) != 0 { panic("writer passed held read lock") }
		if native && runtime.NumGoroutine() > 32 { panic("ordinary goroutine per waiter") }
		rw.RUnlock()
		for i := 0; i < 257; i++ { <-done }
		if atomic.LoadInt32(&stage) != 1 { panic("detached writer lost") }
		if atomic.LoadInt32(&overlapped) != 0 { panic("reader passed pending writer") }
	}
	if n, _ := runtime.BlockProfile(nil); n == 0 { panic("missing block profile") }
	if n, _ := runtime.MutexProfile(nil); n == 0 { panic("missing mutex profile") }
	println("rwmutex complete")
}
`
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/cororwmutex\n\ngo 1.28\n"), 0o666); err != nil {
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
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"disabled-inline", "nocoro", ""},
		{"disabled-noinline", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(tmp, config.name+".exe")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe,
				"-gcflags=internal/sync="+config.flags,
				"-gcflags=sync="+config.flags,
				"-gcflags=example.com/cororwmutex="+config.flags, ".")
			build.Dir = tmp
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			data, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			mode := "ordinary"
			if config.experiment == "coro" {
				mode = "native"
				for _, name := range []string{"readWorker", "writeWorker", "parallelReaders", "mixed", "heldWriter", "lateReader", "spawnWriter", "spawnReaders"} {
					if !strings.Contains(string(data), "func="+name+".coro,") {
						t.Fatalf("%s was not lowered\n%s", name, data)
					}
				}
				nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
				symbols, err := nm.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, symbols)
				}
				for _, name := range []string{"Lock", "Unlock", "RLock", "RUnlock", "rUnlockSlow"} {
					if !strings.Contains(string(symbols), "sync.(*RWMutex)."+name+".coro") {
						t.Fatalf("missing native RWMutex.%s", name)
					}
				}
			}
			for _, procs := range []int{1, 4} {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				run := testenv.CommandContext(t, ctx, exe, mode)
				run.Env = append(run.Environ(), "GOMAXPROCS="+strconv.Itoa(procs))
				data, err := run.CombinedOutput()
				cancel()
				if err != nil || string(data) != "rwmutex complete\n" {
					t.Errorf("P%d: %v\n%s", procs, err, data)
				}
			}
		})
	}
}
