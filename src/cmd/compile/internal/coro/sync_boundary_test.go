// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSynchronizationBoundary(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	for name, source := range map[string]string{
		"go.mod": "module example.com/corosync\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf

import "sync"

func Lock(mu *sync.Mutex) { mu.Lock() }
func Unlock(mu *sync.Mutex) { mu.Unlock() }
func Wait(wg *sync.WaitGroup) { wg.Wait() }
`,
		"main.go": `package main

import (
	"example.com/corosync/leaf"
	"os"
	"runtime"
	"sync"
)

var mu sync.Mutex
var mutexDone chan struct{}

func mutexWaiter() {
	leaf.Lock(&mu)
	leaf.Unlock(&mu)
	mutexDone <- struct{}{}
}

func mutexRoot() {
	mutexDone = make(chan struct{})
	leaf.Lock(&mu)
	go mutexWaiter()
	runtime.Gosched()
	leaf.Unlock(&mu)
	<-mutexDone
	println("mutex complete")
}

var wg sync.WaitGroup
var groupReady chan struct{}

func groupChild() {
	runtime.Gosched()
	groupReady <- struct{}{}
}

func groupRoot() {
	go groupChild()
	leaf.Wait(&wg)
	println("waitgroup complete")
}

func groupController() {
	<-groupReady
	wg.Done()
}

func main() {
	runtime.GOMAXPROCS(1)
	if os.Args[1] == "mutex" {
		mutexRoot()
	} else {
		groupReady = make(chan struct{})
		wg.Add(1)
		go groupController()
		groupRoot()
	}
}
`,
	} {
		path := filepath.Join(tmp, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for _, config := range []struct {
		name, experiment, flags string
	}{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4"},
		{"disabled", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(tmp, config.name+".exe")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe,
				"-gcflags=example.com/corosync/...="+config.flags, ".")
			build.Dir = tmp
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			data, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, data)
			}
			if config.experiment == "coro" {
				out := string(data)
				for _, name := range []string{"mutexRoot", "mutexWaiter", "groupRoot"} {
					want := "coro: skip main." + name + ": "
					if config.name == "noinline" {
						want += "unsupported coroutine dependency"
					}
					if !strings.Contains(out, want) {
						t.Errorf("missing fallback %q\n%s", want, out)
					}
				}
				if !strings.Contains(out, "func=groupChild.coro,") {
					t.Errorf("nonblocking child was not lowered\n%s", out)
				}
				for _, name := range []string{"Lock", "Wait"} {
					want := "coro: func=example.com/corosync/leaf." + name + " effect=may-suspend"
					if !strings.Contains(out, want) {
						t.Errorf("missing exported effect %q\n%s", want, out)
					}
				}
			}
			for _, mode := range []string{"mutex", "waitgroup"} {
				// These cases used to park the sole native executor while its
				// logical child held the work needed to release the wait. Bound
				// the process, rather than leaving a blocked test goroutine.
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				run := testenv.CommandContext(t, ctx, exe, mode)
				data, err := run.CombinedOutput()
				cancel()
				if err != nil || string(data) != mode+" complete\n" {
					t.Errorf("%s: %v\n%s", mode, err, data)
				}
			}
		})
	}
}
