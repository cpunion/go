// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

const invariantReturnSource = `package main
import "runtime"

func first(a bool, p, q *int, k, n int) int {
	if n == 0 { runtime.Gosched(); return *p + k }
	v := second(a, p, q, k, n-1)
	if a { v += *q }
	v++
	return v + *p + k
}
func second(flag bool, p, q *int, amount, depth int) int {
	if depth == 0 { runtime.Gosched(); return *p + amount }
	v := first(flag, p, q, amount, depth-1)
	if flag { v += *q }
	v++
	return v + *p + amount
}
func changed(a bool, v, n int) int {
	if n <= 0 { runtime.Gosched(); return v }
	child := changed(!a, v, n-1)
	if a { child++ }
	return child + v
}
func unequalA(p *int, n int) int {
	if n <= 0 { runtime.Gosched(); return *p }
	return unequalB(p, n-1) + *p
}
func unequalB(p *int, n int) int {
	if n <= 0 { runtime.Gosched(); return *p + 1 }
	return unequalA(p, n-1) + *p
}
func twice(n, k int) int {
	if n <= 0 { runtime.Gosched(); return k }
	x := twice(n-1, k)
	return x + x
}
func nativeCaller(a bool, p, q *int, k, n int) int {
	runtime.Gosched()
	return first(a, p, q, k, n)
}
func nilPanic(n int, p *int) (recovered any) {
	defer func() { recovered = recover() }()
	_ = first(true, p, nil, 1, n)
	return nil
}
func collect(done chan bool) {
	for i := 0; i < 4; i++ { runtime.GC() }
	done <- true
}
func main() {
	done := make(chan bool, 1)
	go collect(done)
	for _, n := range []int{0, 1, 2, 3, 41, 4096} {
		p, q := new(int), new(int)
		*p, *q = n+3, -9
		const k = -6
		if got, want := first(false, p, q, k, n), *p+k+n*(1+*p+k); got != want { panic("first") }
		if got, want := second(true, p, q, k, n), *p+k+n*(*q+1+*p+k); got != want { panic("second") }
		if got, want := nativeCaller(true, p, p, k, n), *p+k+n*(2 * *p+1+k); got != want { panic("native call and alias") }
		if got, want := changed(true, 2, n), 2+n*2+(n+1)/2; got != want { panic("changed argument") }
		if got, want := unequalA(p, n), *p+n * *p+n%2; got != want { panic("unequal base") }
	}
	v := 13
	if twice(7, 3) != 384 { panic("repeated result") }
	if first(true, &v, nil, 1, 0) != 14 { panic("step at zero depth") }
	if first(false, &v, nil, 1, 31) != 479 { panic("untaken step") }
	// Both the public call and native caller must preserve the original nil
	// dereference, after the yield and only when the conditional step is used.
	if nilPanic(0, &v) != nil { panic("unexpected panic") }
	if _, ok := nilPanic(31, &v).(runtime.Error); !ok { panic("missing nil panic") }
	runtime.LockOSThread()
	if first(true, &v, &v, 1, 61) != 14+61*28 { panic("locked thread") }
	runtime.UnlockOSThread()
	<-done
	println("switch-return-ok")
}
`

func TestInvariantReturn(t *testing.T) {
	lowered := []string{"first", "second", "nativeCaller", "changed", "unequalA", "unequalB", "twice"}
	testSwitchReturnFilesNative(t, map[string]string{"main.go": invariantReturnSource}, "", nil, lowered, lowered)

	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(invariantReturnSource), 0o666); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "invariant.exe")
	goTool := testenv.GoToolPath(t)
	build := testenv.Command(t, goTool, "build", "-gcflags=-l -d=coro=4", "-o", program, source)
	build.Env = append(build.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	multiply := regexp.MustCompile(`\s(MADD|MUL|IMULQ)\s`)
	for _, name := range []string{"first", "second", "changed", "unequalA", "unequalB", "twice"} {
		out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", "^main[.]"+name+"[.]coro[.]func1$", program).CombinedOutput()
		if err != nil || len(out) == 0 {
			t.Fatalf("missing native body %s: %v %s", name, err, out)
		}
		want := name == "first" || name == "second"
		if folded := multiply.Match(out); folded != want {
			t.Fatalf("%s invariant return=%v want %v\n%s", name, folded, want, out)
		}
	}
}
