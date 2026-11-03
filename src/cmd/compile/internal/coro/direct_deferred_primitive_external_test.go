// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDirectDeferredPrimitives(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main

import (
	"runtime"
	"sync"
	"time"
)

type payload struct {
	value int
	pad   [2048]byte
}

//go:noinline
func yieldOwner() (result int) {
	defer func() { result++ }()
	defer runtime.Gosched()
	runtime.Gosched()
	return 7
}

//go:noinline
func sleepOwner() (result int) {
	defer func() { result++ }()
	defer time.Sleep(time.Microsecond)
	runtime.Gosched()
	return 9
}

//go:noinline
func gcOwner() (result *payload) {
	result = &payload{value: 23}
	result.pad[2047] = 29
	defer runtime.GC()
	runtime.Gosched()
	return
}

//go:noinline
func closeOwner(ch chan int) {
	original := ch
	defer close(ch)
	ch = make(chan int)
	original <- 31
	runtime.Gosched()
}

//go:noinline
func unlockOwner(mu *sync.Mutex) {
	mu.Lock()
	defer mu.Unlock()
	mu = new(sync.Mutex)
	runtime.Gosched()
}

//go:noinline
func rwUnlockOwner(mu *sync.RWMutex) {
	mu.Lock()
	defer mu.Unlock()
	mu = new(sync.RWMutex)
	runtime.Gosched()
}

//go:noinline
func rUnlockOwner(mu *sync.RWMutex) {
	mu.RLock()
	defer mu.RUnlock()
	mu = new(sync.RWMutex)
	runtime.Gosched()
}

//go:noinline
func doneOwner(wg *sync.WaitGroup) {
	defer wg.Done()
	wg = new(sync.WaitGroup)
	runtime.Gosched()
}

//go:noinline
func panicOwner() (result int) {
	defer func() {
		if recover() != 73 {
			panic("panic identity")
		}
		result = 73
	}()
	defer runtime.GC()
	panic(73)
}

//go:noinline
func panicBuiltinOwner() (result int) {
	defer func() {
		if recover() != 97 {
			panic("deferred panic identity")
		}
		result = 97
	}()
	value := any(97)
	defer panic(value)
	value = 98
	runtime.Gosched()
	return
}

//go:noinline
func recoverBuiltinOwner() (result int) {
	defer func() {
		if recover() != 101 {
			panic("deferred recover identity")
		}
		result = 101
	}()
	defer recover()
	panic(101)
}

//go:noinline
func deferredExitOwner(done chan int) {
	defer func() { done <- 83 }()
	defer runtime.Goexit()
	runtime.Gosched()
}

func controls() {
	if yieldOwner() != 8 || sleepOwner() != 10 {
		panic("named result or cleanup")
	}
	p := gcOwner()
	if p.value != 23 || p.pad[2047] != 29 {
		panic("GC root")
	}
	ch := make(chan int)
	go closeOwner(ch)
	if value, ok := <-ch; value != 31 || !ok {
		panic("close send")
	}
	if _, ok := <-ch; ok {
		panic("close cleanup")
	}
	mu := new(sync.Mutex)
	unlockOwner(mu)
	if !mu.TryLock() {
		panic("unlock cleanup")
	}
	mu.Unlock()
	rw := new(sync.RWMutex)
	rwUnlockOwner(rw)
	if !rw.TryLock() {
		panic("RWMutex unlock cleanup")
	}
	rw.Unlock()
	rUnlockOwner(rw)
	if !rw.TryLock() {
		panic("RWMutex read unlock cleanup")
	}
	rw.Unlock()
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go doneOwner(wg)
	wg.Wait()
	if panicOwner() != 73 {
		panic("recover cleanup")
	}
	if panicBuiltinOwner() != 97 || recoverBuiltinOwner() != 101 {
		panic("deferred builtin cleanup")
	}
	done := make(chan int)
	go deferredExitOwner(done)
	if <-done != 83 {
		panic("Goexit cleanup")
	}
}

func main() {
 controls()
 runtime.LockOSThread()
 controls()
 runtime.UnlockOSThread()
 println("direct-deferred-primitives-ok")
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"native-inline", "coro", "-d=coro=4"},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2"},
		{"native-no-dwarf", "coro", "-d=coro=4 -dwarf=false -gendwarfinl=0"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			file, exe := filepath.Join(dir, "main.go"), filepath.Join(dir, "program")
			if err := os.WriteFile(file, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, file)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"yieldOwner", "sleepOwner", "gcOwner", "closeOwner", "unlockOwner", "rwUnlockOwner", "rUnlockOwner", "doneOwner", "panicOwner", "panicBuiltinOwner", "recoverBuiltinOwner", "deferredExitOwner"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
							t.Fatal("direct deferred primitive lost native admission")
						}
					})
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				if out, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "direct-deferred-primitives-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
