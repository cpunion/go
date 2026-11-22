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
//go:noinline
func readLeftDepth(buf *[448]byte) int { return int(buf[445]) }
//go:noinline
func readRightDepth(buf *[512]byte) int { return int(buf[503]) }

func leftDepth(fixed uint16, p *int, n int) int {
	var buf [448]byte
	buf[445] = byte(n)
	if n == 0 { runtime.Gosched(); return int(fixed) + *p + readLeftDepth(&buf) }
	child := rightDepth(fixed, p, n-1)
	return child + int(fixed) + *p + readLeftDepth(&buf)
}
func rightDepth(fixed uint16, p *int, n int) int {
	var buf [512]byte
	buf[503] = byte(n)
	if n == 0 { runtime.Gosched(); return int(fixed) + *p + readRightDepth(&buf) }
	child := leftDepth(fixed, p, n-1)
	return child + int(fixed) + *p + readRightDepth(&buf)
}
func nilDepth(n int) (recovered any) {
	defer func() { recovered = recover() }()
	_ = leftDepth(255, nil, n)
	return nil
}
func projectedDepth(addByte bool, fixed int, n int) int {
	if n <= 0 { runtime.Gosched(); return fixed }
	result := projectedDepth(addByte, fixed, n-1)
	if addByte { result += int(byte(n)) }
	return result + n
}
func offsetDepth(p *int, n int) int {
	var buf [448]byte
	buf[445] = byte(n+1)
	if n == 0 { runtime.Gosched(); return readLeftDepth(&buf) + *p }
	return offsetDepth(p, n-1) + readLeftDepth(&buf) + *p
}
var readerCalls int
//go:noinline
func readWithEffect(buf *[448]byte) int {
	readerCalls++
	return int(buf[445])
}
func effectDepth(p *int, n int) int {
	var buf [448]byte
	buf[445] = byte(n)
	if n == 0 { runtime.Gosched(); return readWithEffect(&buf) + *p }
	return effectDepth(p, n-1) + readWithEffect(&buf) + *p
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
	for _, n := range []int{0, 1, 254, 255, 256, 257, 511, 512, 513, 4096} {
		for _, p := range []int{0, -67, int(^uint(0)>>1)} {
			var want, offset, projected int
			for i := 0; i <= n; i++ {
				want += 65535 + p + int(byte(i))
				offset += p + int(byte(i+1))
				projected += i + int(byte(i))
			}
			if got := leftDepth(65535, &p, n); got != want { panic("wrapped byte depth") }
			if got := rightDepth(65535, &p, n); got != want { panic("different private array") }
			if got := projectedDepth(true, p, n); got != p+projected { panic("conditioned byte depth") }
			if got := projectedDepth(false, p, n); got != p+n*(n+1)/2 { panic("untaken byte depth") }
			if got := offsetDepth(&p, n); got != offset { panic("changed array write") }
			readerCalls = 0
			if got := effectDepth(&p, n); got != want-(n+1)*65535 || readerCalls != n+1 {
				panic("effectful reader skipped")
			}
		}
	}
	if projectedDepth(true, 7, -42) != 7 { panic("negative depth fallback") }
	if _, ok := nilDepth(513).(runtime.Error); !ok { panic("missing array-path nil panic") }
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
	if leftDepth(20, &v, 257) != 258*33+32641 { panic("ordinary array recursion") }
	runtime.UnlockOSThread()
	<-done
	println("switch-return-ok")
}
`

func TestInvariantReturn(t *testing.T) {
	lowered := []string{"first", "second", "nativeCaller", "changed", "unequalA", "unequalB", "twice",
		"leftDepth", "rightDepth", "projectedDepth", "offsetDepth", "effectDepth"}
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
	for _, name := range []string{"first", "second", "changed", "unequalA", "unequalB", "twice",
		"leftDepth", "rightDepth", "projectedDepth"} {
		out, err := testenv.Command(t, goTool, "tool", "objdump", "-s", "^main[.]"+name+"[.]coro[.]func1$", program).CombinedOutput()
		if err != nil || len(out) == 0 {
			t.Fatalf("missing native body %s: %v %s", name, err, out)
		}
		want := name == "first" || name == "second" ||
			name == "leftDepth" || name == "rightDepth" || name == "projectedDepth"
		if folded := multiply.Match(out); folded != want {
			t.Fatalf("%s invariant return=%v want %v\n%s", name, folded, want, out)
		}
	}
}
