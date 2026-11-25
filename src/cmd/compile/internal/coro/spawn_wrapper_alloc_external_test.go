// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSpawnElidedWrapperNoCaptureCells(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	files := map[string]string{
		"go.mod": "module example.com/spawnwrapper\n\ngo 1.28\n",
		"main.go": `package main

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

type box struct{ add int }

//go:noinline
func staticWorker(id int, b *box, out chan int) {
	runtime.Gosched()
	out <- id*10 + b.add
}

//go:noinline
func spawnStaticLoop(batches, n int, b *box, out chan int) int {
	sum := 0
	for range batches {
		for i := 0; i < n; i++ {
			x := i
			go staticWorker(x, b, out)
			x = -999
			_ = x
		}
		for i := 0; i < n; i++ {
			sum += <-out
		}
	}
	return sum
}

//go:noinline
func dynA(x int, out chan int) {
	runtime.Gosched()
	out <- x + 100
}

//go:noinline
func dynB(x int, out chan int) {
	runtime.Gosched()
	out <- x + 200
}

//go:noinline
func spawnDynamicLoop(n int) int {
	out := make(chan int, n)
	for i := 0; i < n; i++ {
		fn := dynA
		if i%2 == 1 {
			fn = dynB
		}
		x := i
		go fn(x, out)
		fn = nil
		x = -1
		_ = fn
		_ = x
	}
	sum := 0
	for i := 0; i < n; i++ {
		sum += <-out
	}
	return sum
}

type runner interface {
	Run(x int, out chan int)
}

type addRunner struct{ delta int }

//go:noinline
func (r *addRunner) Run(x int, out chan int) {
	runtime.Gosched()
	out <- x + r.delta
}

type mulRunner struct{ scale int }

//go:noinline
func (r *mulRunner) Run(x int, out chan int) {
	runtime.Gosched()
	out <- x * r.scale
}

//go:noinline
func spawnOpenLoop(n int) int {
	out := make(chan int, n)
	a := &addRunner{delta: 7}
	m := &mulRunner{scale: 3}
	for i := 0; i < n; i++ {
		var r runner = a
		if i%2 == 1 {
			r = m
		}
		x := i
		go r.Run(x, out)
		r = nil
		x = -1
		_ = r
		_ = x
	}
	sum := 0
	for i := 0; i < n; i++ {
		sum += <-out
	}
	return sum
}

//go:noinline
func callbackWorker(gate <-chan struct{}, fn func() int, out chan int) {
	<-gate
	out <- fn()
}

//go:noinline
func spawnUserClosures(n int) (int, int) {
	gate := make(chan struct{})
	out1 := make(chan int, n)
	out2 := make(chan int, n)
	for i := 0; i < n; i++ {
		v := i * 2
		go func() {
			<-gate
			out1 <- v
		}()
		v += 1 // closure observes mutation in its own iteration cell after gate closes
		u := i * 5
		go callbackWorker(gate, func() int { return u }, out2)
		u += 2 // nested closure argument observes its own iteration cell after gate closes
	}
	close(gate)
	s1, s2 := 0, 0
	for i := 0; i < n; i++ {
		s1 += <-out1
		s2 += <-out2
	}
	return s1, s2
}

func main() {
	const n = 32
	b := &box{add: 3}
	out := make(chan int, n)
	wantStatic := 0
	for i := 0; i < n; i++ {
		wantStatic += i*10 + 3
	}
	if got := spawnStaticLoop(1, n, b, out); got != wantStatic {
		panic(fmt.Sprintf("static sum = %d, want %d", got, wantStatic))
	}

	wantDyn := 0
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			wantDyn += i + 100
		} else {
			wantDyn += i + 200
		}
	}
	if got := spawnDynamicLoop(n); got != wantDyn {
		panic(fmt.Sprintf("dynamic sum = %d, want %d", got, wantDyn))
	}

	wantOpen := 0
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			wantOpen += i + 7
		} else {
			wantOpen += i * 3
		}
	}
	if got := spawnOpenLoop(n); got != wantOpen {
		panic(fmt.Sprintf("open sum = %d, want %d", got, wantOpen))
	}

	wantC1, wantC2 := 0, 0
	for i := 0; i < n; i++ {
		wantC1 += i*2 + 1
		wantC2 += i*5 + 2
	}
	if got1, got2 := spawnUserClosures(n); got1 != wantC1 || got2 != wantC2 {
		panic(fmt.Sprintf("closures = (%d, %d), want (%d, %d)", got1, got2, wantC1, wantC2))
	}

	if os.Getenv("CHECK_ZERO_ALLOC") == "1" {
		runtime.GOMAXPROCS(1)
		// Warm scheduler, root frame, task cache, and record cache.
		for range 4 {
			if got := spawnStaticLoop(4, n, b, out); got != 4*wantStatic {
				panic(fmt.Sprintf("warm static sum = %d, want %d", got, 4*wantStatic))
			}
		}
		a1 := testing.AllocsPerRun(20, func() {
			if got := spawnStaticLoop(1, n, b, out); got != wantStatic {
				panic(fmt.Sprintf("alloc1 static sum = %d, want %d", got, wantStatic))
			}
		})
		a10 := testing.AllocsPerRun(20, func() {
			if got := spawnStaticLoop(10, n, b, out); got != 10*wantStatic {
				panic(fmt.Sprintf("alloc10 static sum = %d, want %d", got, 10*wantStatic))
			}
		})
		// Additional batches of 32 spawns must not add per-spawn capture-cell allocations.
		if a10-a1 > 2 {
			panic(fmt.Sprintf("spawnStaticLoop extra 9 batches (288 spawns) added %v allocs (a1=%v a10=%v), want 0", a10-a1, a1, a10))
		}
	}
}
`,
	}
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	hasNative := runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" ||
		runtime.GOOS == "linux" && runtime.GOARCH == "amd64"
	configs := []struct {
		name       string
		experiment string
		flags      []string
		checkAsm   bool
		checkAlloc bool
	}{
		{"coro-noinline", "coro", []string{"-gcflags=-l -d=coro=4"}, true, hasNative},
		{"coro-inline", "coro", []string{"-gcflags=-d=coro=4"}, false, false},
		{"coro-checkptr", "coro", []string{"-gcflags=-l -d=coro=4 -d=checkptr=2"}, true, false},
		{"nocoro", "nocoro", []string{"-gcflags=-l"}, false, false},
	}
	if platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
		configs = append(configs, struct {
			name       string
			experiment string
			flags      []string
			checkAsm   bool
			checkAlloc bool
		}{"coro-race", "coro", []string{"-race"}, false, false})
	}
	for _, cfg := range configs {
		t.Run(cfg.name, func(t *testing.T) {
			exe := filepath.Join(dir, cfg.name)
			args := append([]string{"build"}, cfg.flags...)
			args = append(args, "-o", exe, ".")
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Dir = dir
			build.Env = append(build.Environ(), "GOEXPERIMENT="+cfg.experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if cfg.checkAsm {
				dump := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump", "-s", `main\.spawnStaticLoop\.coro\.func1$`, exe)
				out, err := dump.CombinedOutput()
				if err != nil {
					t.Fatalf("objdump: %v\n%s", err, out)
				}
				text := string(out)
				if !strings.Contains(text, "runtime.coroSpawnFrame(SB)") {
					t.Fatalf("missing runtime.coroSpawnFrame in spawnStaticLoop.coro.func1:\n%s", text)
				}
				if strings.Contains(text, "runtime.mallocgc") || strings.Contains(text, "runtime.newobject") {
					t.Fatalf("unexpected heap allocation in elided-wrapper spawnStaticLoop.coro.func1:\n%s", text)
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				env := append(run.Environ(), "GOMAXPROCS="+p)
				if cfg.checkAlloc && p == "1" {
					env = append(env, "CHECK_ZERO_ALLOC=1")
				}
				run.Env = env
				if out, err := run.CombinedOutput(); err != nil {
					t.Fatalf("P=%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
