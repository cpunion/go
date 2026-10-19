// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestExpressionSwitch(t *testing.T) {
	const source = `package main
import "runtime"

var trace string
func mark(s string, value int) int { trace += s; return value }

//go:noinline
func dispatch(x int) int {
	switch mark("t", x) {
	default:
		runtime.Gosched()
		return -1
	case mark("a", 1), mark("b", 2):
		runtime.Gosched()
		return 10
	case mark("c", 3):
		runtime.Gosched()
		return 30
	case mark("d", 4):
	}
	return 40
}

var calls int
//go:noinline
func awaited(x int) int { calls++; runtime.Gosched(); return x }

//go:noinline
func initialized(x int) int {
	switch v := awaited(x); v {
	case 1:
		return awaited(v) + 10
	case 2:
		switch v + 1 {
		case 3:
			return awaited(v) + 20
		}
	}
	return 30
}

//go:noinline
func looping() int {
	n := 0
	for i := 0; i < 4; i++ {
		switch i {
		case 0:
			runtime.Gosched()
			continue
		case 1:
			runtime.Gosched()
			break
		case 2:
			for j := 0; j < 3; j++ {
				runtime.Gosched()
				if j == 1 { break }
				n += 10
			}
		default:
			n += 100
		}
		n++
	}
	return n
}

var cleanups int
func record(n int) { cleanups = cleanups*10 + n }

//go:noinline
func deferred(x int) {
	for i := 1; i <= 3; i++ {
		switch x {
		case 1:
			defer record(i)
			runtime.Gosched()
		default:
			return
		}
	}
}

//go:noinline
func pointerCase(x int) *int {
	switch x {
	case 1:
		v := new(int)
		*v = 73
		runtime.Gosched()
		runtime.GC()
		return v
	default:
		runtime.Gosched()
		return nil
	}
}

//go:noinline
func interfaceCase(x any) int {
	switch x {
	case "hello":
		runtime.Gosched()
		return 1
	case 42:
		runtime.Gosched()
		return 2
	default:
		return 3
	}
}

//go:noinline
func comparisonPanic() {
	defer record(4)
	var x any = []int{1}
	switch x {
	case x:
		runtime.Gosched()
	}
}

func recoverComparison() {
	defer func() { if recover() == nil { panic("missing comparison panic") } }()
	comparisonPanic()
}

//go:noinline
func suspendingTag() int {
	switch awaited(1) { case 1: runtime.Gosched(); return 1 }
	return 0
}

//go:noinline
func suspendingCase() int {
	switch 1 { case awaited(1): runtime.Gosched(); return 1 }
	return 0
}

//go:noinline
func falling() int {
	switch 1 {
	case 1:
		runtime.Gosched()
		fallthrough
	default:
		return 5
	}
}

func main() {
	for _, test := range []struct { x, result int; trace string }{
		{0, -1, "tabcd"}, {1, 10, "ta"}, {2, 10, "tab"},
		{3, 30, "tabc"}, {4, 40, "tabcd"},
	} {
		trace = ""
		if dispatch(test.x) != test.result || trace != test.trace { panic("dispatch repeated or reordered") }
	}
	if initialized(1) != 11 || initialized(2) != 22 || initialized(3) != 30 || calls != 5 { panic("switch initialization") }
	if looping() != 113 { panic("switch loop target") }
	deferred(1)
	if cleanups != 321 { panic("case cleanup order") }
	if pointerCase(0) != nil || *pointerCase(1) != 73 { panic("case pointer lifetime") }
	if interfaceCase("hello") != 1 || interfaceCase(42) != 2 || interfaceCase(false) != 3 { panic("interface dispatch") }
	cleanups = 0
	recoverComparison()
	if cleanups != 4 { panic("comparison panic cleanup") }
	if suspendingTag() != 1 || suspendingCase() != 1 || falling() != 5 { panic("ordinary fallback") }
	println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"suspendingTag", "coroutine site in switch header"},
			{"suspendingCase", "coroutine site in switch header"},
			{"falling", "control flow fallthrough"},
		},
		[]string{"dispatch", "initialized", "looping", "deferred", "pointerCase", "interfaceCase", "comparisonPanic"})
}
