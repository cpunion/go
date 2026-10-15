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

func TestEscapingFrameLocals(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"

var saved *int

func address(n int) *int { x := n; runtime.Gosched(); return &x }
func parameter(n int) *int { runtime.Gosched(); return &n }
func named(n int) (x int, p *int) { x = n; runtime.Gosched(); return x, &x }
func namedBare(n int) (x int, p *int) { x = n; p = &x; runtime.Gosched(); return }
func boxed(n int) any { x := n; runtime.Gosched(); return &x }
func awaited(n int) *int { _, p := named(n); return p }
func slice(n int) []int { var x [4]int; x[2] = n; runtime.Gosched(); return x[:] }
func publish(n int) { x := n; runtime.Gosched(); saved = &x }

//go:noinline
func identity(p *int) *int { return p }

func indirect(n int) *int { x := n; p := &x; runtime.Gosched(); return identity(p) }
func plainAddress(n int) *int { x := n; return &x }
func inlined(n int) *int { runtime.Gosched(); return plainAddress(n) }

func loop(n int) []*int {
	var out []*int
	for i := 0; i < n; i++ {
		x := i
		runtime.Gosched()
		out = append(out, &x)
	}
	return out
}

func sent(out chan *int, n int) { x := n; runtime.Gosched(); out <- &x }

func recursive(out *[]*int, n int) {
	x := n
	if n > 0 { recursive(out, n-1) } else { runtime.Gosched() }
	*out = append(*out, &x)
}

func main() {
	var pointers []*int
	var slices [][]int
	var values []int
	out := make(chan *int, 1)
	for i := 0; i < 20; i++ {
		pointers = append(pointers, address(i), parameter(i), indirect(i))
		x, p := named(i)
		if x != i { panic("named result") }
		pointers = append(pointers, p)
		x, p = namedBare(i)
		if x != i { panic("bare named result") }
		pointers = append(pointers, p, boxed(i).(*int), awaited(i), inlined(i))
		publish(i)
		pointers = append(pointers, saved)
		sent(out, i)
		pointers = append(pointers, <-out)
		for j := 0; j < 10; j++ { values = append(values, i) }
		slices = append(slices, slice(i))
		runtime.GC()
	}
	for i, p := range pointers {
		if *p != values[i] { println("pointer", i, *p, values[i]); panic("escaped pointer reused") }
		for j := 0; j < i; j++ { if p == pointers[j] { panic("escaped pointer alias") } }
	}
	for i, s := range slices { if s[2] != i { panic("escaped array reused") } }
	for round := 0; round < 2; round++ {
		for i, p := range loop(40) { if *p != i { panic("loop cell reused") } }
		var frames []*int
		recursive(&frames, 64)
		for i, p := range frames { if *p != i { panic("recursive cell reused") } }
	}
	println("escaping-frame-locals-ok")
}
`
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, experiment, flags string }{
		{"coro/inline", "coro", "-d=coro=4 -S"},
		{"coro/noinline", "coro", "-l -d=coro=4 -S"},
		{"coro/checkptr", "coro", "-l -d=coro=4,checkptr=2 -S"},
		{"nocoro", "nocoro", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "heap.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+test.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			assembly, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, assembly)
			}
			if test.experiment == "coro" {
				for _, name := range []string{"address", "parameter", "named", "namedBare", "boxed", "awaited", "slice", "publish", "indirect", "inlined", "loop", "sent", "recursive"} {
					if !strings.Contains(string(assembly), "main."+name+".coro STEXT") {
						t.Errorf("missing lowered %s", name)
					}
				}
				if !strings.Contains(string(assembly), "runtime.coroTakeSelfFrame(SB)") {
					t.Error("recursive function did not select cached self frames")
				}
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOGC=1", "GOMAXPROCS=1")
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "escaping-frame-locals-ok" {
				t.Fatalf("execution = (%v, %q), want escaping-frame-locals-ok", err, out)
			}
		})
	}
}
