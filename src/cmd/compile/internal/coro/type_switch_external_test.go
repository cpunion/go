// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestTypeSwitch(t *testing.T) {
	const source = `package main
import "runtime"

type first interface { First() }
type second interface { Second() }
type both struct { n int }
func (*both) First() {}
func (*both) Second() {}

var evaluations int
//go:noinline
func evaluate(x any) any { evaluations++; return x }
//go:noinline
func awaited(x any) any { runtime.Gosched(); return x }

//go:noinline
func concrete(input any) int {
	switch x := evaluate(input).(type) {
	default:
		runtime.Gosched()
		if x != input { panic("default binding") }
		return 90
	case nil:
		runtime.Gosched()
		if x != nil { panic("nil binding") }
		return 10
	case *both:
		runtime.Gosched()
		if x == nil { return 20 }
		return x.n
	case int:
		return awaited(x).(int) + x
	case string, bool:
		runtime.Gosched()
		if x != input { panic("multiple type binding") }
		return 30
	}
}

//go:noinline
func interfaceOrder(input any) int {
	switch x := input.(type) {
	case first:
		runtime.Gosched()
		if any(x) != input { panic("interface binding") }
		return 1
	case *both:
		runtime.Gosched()
		return x.n
	case second:
		runtime.Gosched()
		return 3
	}
	return 4
}

//go:noinline
func concreteOrder(input first) int {
	switch x := input.(type) {
	case *both:
		runtime.Gosched()
		return x.n
	case first:
		runtime.Gosched()
		if x != input { panic("nonempty interface binding") }
		return 2
	default:
		runtime.Gosched()
		if x != nil { panic("nonempty default binding") }
		return 3
	}
}

//go:noinline
func initialized(input any) int {
	switch v := awaited(input); x := v.(type) {
	case int:
		switch x % 2 {
		case 0: return awaited(x).(int) + x
		default: runtime.Gosched(); return x
		}
	default: runtime.Gosched(); return 9
	}
}

var gcRequest = make(chan struct{})
var gcDone = make(chan struct{})
func collectGC() {
	for range gcRequest { runtime.GC(); gcDone <- struct{}{} }
}
var captures [3]*int
var cleanups int

//go:noinline
func captured() (result int) {
	for i := 0; i < 3; i++ {
		switch x := any(i + 1).(type) {
		case int:
			captures[i] = &x
			defer func() { cleanups = cleanups*10 + x }()
			runtime.Gosched()
			x += 10
			gcRequest <- struct{}{}
			<-gcDone
			if i == 0 { continue }
			if i == 1 { break }
			result += x
		default: panic("int dispatch")
		}
		result++
	}
	return
}

//go:noinline
func noBinding(input any) int {
	switch input.(type) {
	case int: runtime.Gosched(); return 1
	case string: runtime.Gosched(); break
	}
	return 2
}
//go:noinline
func empty(input any) int {
	runtime.Gosched()
	switch input.(type) {}
	return 3
}
func send(ch chan int, x int) { runtime.Gosched(); ch <- x }
//go:noinline
func spawned(input any, ch chan int) int {
	switch x := input.(type) {
	case int: go send(ch, x); value := <-ch; return value
	}
	return 0
}
//go:noinline
func suspendingGuard(input any) int {
	switch x := awaited(input).(type) {
	case int: runtime.Gosched(); return x
	}
	return 0
}
//go:noinline
func escapingClosures() [3]func() int {
	var fs [3]func() int
	for i := 0; i < 3; i++ {
		switch x := any(i+1).(type) {
		case int:
			fs[i] = func() int { return x }
			runtime.Gosched()
			x += 10
		}
	}
	return fs
}

func main() {
	go collectGC()
	for i := 0; i < 2; i++ {
		if concrete(nil) != 10 || concrete((*both)(nil)) != 20 || concrete(&both{41}) != 41 ||
			concrete(21) != 42 || concrete("hello") != 30 || concrete(true) != 30 || concrete(1.5) != 90 { panic("concrete dispatch") }
	}
	if evaluations != 14 { panic("guard reevaluated") }
	b := &both{42}
	if interfaceOrder(b) != 1 || interfaceOrder(nil) != 4 || concreteOrder(b) != 42 || concreteOrder(nil) != 3 { panic("interface order") }
	if initialized(20) != 40 || initialized(21) != 21 || initialized(nil) != 9 { panic("initializer or nested switch") }
	if captured() != 15 || cleanups != 1431 { panic("case defer or branch") }
	if captures[0] == captures[1] || captures[1] == captures[2] || *captures[0] != 11 || *captures[1] != 12 || *captures[2] != 13 { panic("case variable lifetime") }
	if noBinding(1) != 1 || noBinding("hello") != 2 || noBinding(nil) != 2 || empty(nil) != 3 { panic("unbound dispatch") }
	if spawned(42, make(chan int)) != 42 { panic("spawned call") }
	if suspendingGuard(42) != 42 { panic("suspending guard fallback") }
	fs := escapingClosures()
	if fs[0]() != 11 || fs[1]() != 12 || fs[2]() != 13 { panic("ordinary closure capture fallback") }
	close(gcRequest)
	println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, []switchFallback{
		{"suspendingGuard", "coroutine site in switch header"},
		{"escapingClosures", "type switch variable captured by closure"},
	}, []string{
		"concrete", "interfaceOrder", "concreteOrder", "initialized", "captured", "noBinding", "empty", "spawned",
	})
}
