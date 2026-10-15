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

func TestInvocationLocalLifetime(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" ||
		runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("native coroutine executor is not supported")
	}
	tmp := t.TempDir()
	source := filepath.Join(tmp, "main.go")
	const program = `package main

import (
	"runtime"
	"sync/atomic"
)

type result struct { n int; value byte; err error }
type state struct { entered uint64; done chan result }
var current *state
var errValue error = failure(0)
type failure int
func (failure) Error() string { return "invalid" }

func validate(results [2]result) error {
	var first, second bool
	for _, r := range results {
		if r.err != nil { return r.err }
		if r.n != 1 { return errValue }
		switch r.value {
		case '1': first = true
		case '2': second = true
		default: return errValue
		}
	}
	if !first || !second { return errValue }
	return nil
}

func worker1() { atomic.AddUint64(&current.entered, 1); current.done <- result{1, '1', nil} }
func worker2() { atomic.AddUint64(&current.entered, 1); current.done <- result{1, '2', nil} }
func wait(entered *uint64, n uint64) { for atomic.LoadUint64(entered) < n { runtime.Gosched() } }

//go:noinline
func check(p *int) error { if *p != 42 { return errValue }; return nil }

func readResults(p *int) error {
	s := &state{done: make(chan result, 2)}
	current = s
	go worker1()
	wait(&s.entered, 1)
	go worker2()
	wait(&s.entered, 2)
	err := check(p)
	if err == nil {
		first := <-s.done
		second := <-s.done
		results := [2]result{first, second}
		err = validate(results)
	}
	return err
}

func address(v int) *int { local := v; return &local }
func capture(v int) func() int { local := v; return func() int { return local } }

func pointers(v int) (*int, func() int) {
	runtime.Gosched()
	p := address(v)
	f := capture(v + 1)
	return p, f
}

func pair(v int) (int, int) { return v, v + 1 }

func branch(v int) (int, int) {
	if v > 0 {
		runtime.Gosched()
		a, b := pair(v)
		return b, a
	}
	a, b := pair(-v)
	return a, b
}

func iterations(n int) int {
	total := 0
	for i := 0; i < n; i++ {
		a, b := pair(i)
		runtime.Gosched()
		total += a + b
	}
	return total
}

func deferred(result *int) {
	runtime.Gosched()
	v := 37
	defer func(x int) { *result = x }(v)
}

func main() {
	x := 42
	var savedPointers []*int
	var savedClosures []func() int
	for i := 0; i < 10; i++ {
		if err := readResults(&x); err != nil { panic(err) }
		a, b := branch(i + 1)
		if a != i + 2 || b != i + 1 { panic("positive branch") }
		a, b = branch(-i)
		if a != i || b != i + 1 { panic("negative branch") }
		p, f := pointers(i)
		savedPointers = append(savedPointers, p)
		savedClosures = append(savedClosures, f)
		runtime.GC()
		if *p != i || f() != i + 1 { panic("escaped local") }
	}
	runtime.GC()
	for i, p := range savedPointers {
		if *p != i || savedClosures[i]() != i + 1 { panic("reused escaped local") }
	}
	x = 0
	if readResults(&x) != errValue { panic("error branch") }
	if iterations(10) != 100 { panic("loop lifetime") }
	deferred(&x)
	if x != 37 { panic("deferred argument") }
	println("coro-invocation-locals-ok")
}
`
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"inline", "noinline", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			flags, experiment := "-d=coro=4 -m=2 -S", "coro"
			if mode == "noinline" {
				flags += " -l"
			} else if mode == "disabled" {
				flags, experiment = "-m=2", "nocoro"
			}
			exe := filepath.Join(tmp, mode)
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-o", exe,
				"-gcflags=command-line-arguments="+flags, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("building invocation-local program: %v\n%s", err, out)
			}
			text := string(out)
			if mode != "disabled" {
				for _, name := range []string{"readResults", "pointers", "branch", "iterations", "deferred"} {
					if !strings.Contains(text, "func="+name+".coro,") {
						t.Errorf("missing lowered %s", name)
					}
				}
			}
			if mode == "inline" {
				if !strings.Contains(text, "inlining call to validate") ||
					!strings.Contains(text, "inlining call to address") {
					t.Fatal("regression helpers were not inlined")
				}
				start := strings.Index(text, "main.readResults.coro.func1 STEXT")
				if start < 0 {
					t.Fatal("missing readResults resume assembly")
				}
				assembly := text[start:]
				if end := strings.Index(assembly, "\nmain."); end >= 0 {
					assembly = assembly[:end]
				}
				if strings.Contains(assembly, "runtime.wbMove") {
					t.Fatalf("suffix array still requires persistent-frame copying:\n%s", assembly)
				}
				t.Log(strings.SplitN(assembly, "\n", 2)[0])
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOGC=1")
			if out, err := cmd.CombinedOutput(); err != nil ||
				!strings.Contains(string(out), "coro-invocation-locals-ok") {
				t.Fatalf("running invocation-local program: %v\n%s", err, out)
			}
		})
	}
}
