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

func TestInlineCallBoundaries(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" ||
		runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("native coroutine executor is not supported")
	}
	tmp := t.TempDir()
	src := filepath.Join(tmp, "main.go")
	const program = `package main

import "runtime"

var calls int

//go:noinline
func leaf(x int) int {
	if x < 0 { panic("negative") }
	return x
}

func single(x int) int { return leaf(x) }

//go:noinline
func returnSingle(x int) int { return single(x) }

//go:noinline
func assignSingle(x int) int { y := single(x); return y }

//go:noinline
func pairLeaf(x int) (int, bool) {
	calls++
	if x < 0 { panic("negative") }
	return x, true
}

func pair(x int) (int, bool) { return pairLeaf(x) }

func conditional(x int) (int, bool) {
	if x == 0 { return 42, false }
	return pairLeaf(x)
}

//go:noinline
func returnPair(x int) (int, bool) { return pair(x) }

//go:noinline
func returnConditional(x int) (int, bool) { return conditional(x) }

//go:noinline
func yield() { runtime.Gosched() }

func addOne(x int) int { return x + 1 }

type value int

func (v value) method() int { return leaf(int(v)) }

var methodExpression = (*value).method

func main() {
	if returnSingle(17) != 17 || assignSingle(19) != 19 {
		panic("single result")
	}
	x, ok := returnPair(23)
	if x != 23 || !ok || calls != 1 { panic("pair result") }
	x, ok = returnConditional(0)
	if x != 42 || ok || calls != 1 { panic("conditional call") }
	x, ok = returnConditional(29)
	if x != 29 || !ok || calls != 2 { panic("conditional result") }
	yield()
	if addOne(30) != 31 { panic("plain inline") }
	v := value(37)
	if methodExpression(&v) != 37 { panic("method wrapper") }
	println("coro-inline-ok")
}
`
	if err := os.WriteFile(src, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		experiment string
		flags      string
		lower      bool
		race       bool
	}{
		{name: "lower", experiment: "coro", flags: "-d=coro=4 -m=2", lower: true},
		{name: "analysis", experiment: "coro", flags: "-d=coro=2 -m=2"},
		{name: "disabled", experiment: "nocoro", flags: "-m=2"},
		{name: "race", experiment: "coro", flags: "-d=coro=4 -m=2", race: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.race {
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is not supported")
				}
				testenv.MustHaveCGO(t)
			}
			exe := filepath.Join(tmp, test.name)
			args := []string{"build", "-o", exe, "-gcflags=command-line-arguments=" + test.flags}
			if test.race {
				args = append(args, "-race")
			}
			cmd := testenv.Command(t, testenv.GoToolPath(t), append(args, src)...)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment,
				"GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("building default-inline program: %v\n%s", err, out)
			}
			text := string(out)
			if !strings.Contains(text, "inlining call to addOne") {
				t.Fatalf("plain call was not inlined:\n%s", text)
			}
			if test.lower {
				for _, name := range []string{"single", "returnSingle", "assignSingle", "pair", "conditional", "returnPair", "returnConditional", "yield"} {
					if !strings.Contains(text, "func="+name+".coro,") {
						t.Errorf("missing lowered %s:\n%s", name, text)
					}
				}
				if strings.Contains(text, "inlining call to runtime.Gosched") {
					t.Errorf("operation entry was inlined:\n%s", text)
				}
			} else {
				if !strings.Contains(text, "inlining call to single") ||
					strings.Contains(text, "coro: phase=lower") {
					t.Errorf("inactive lowering changed inlining:\n%s", text)
				}
				// Instrumented builds already prohibit inlining runtime code.
				if !test.race && !strings.Contains(text, "inlining call to runtime.Gosched") {
					t.Errorf("inactive lowering preserved an operation entry:\n%s", text)
				}
			}
			cmd = testenv.Command(t, exe)
			if out, err := cmd.CombinedOutput(); err != nil ||
				!strings.Contains(string(out), "coro-inline-ok") {
				t.Fatalf("running default-inline program: %v\n%s", err, out)
			}
		})
	}
}
