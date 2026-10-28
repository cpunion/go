// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestExplicitFrameZeroInitialization(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"

var gcRequest = make(chan bool)
var gcDone = make(chan bool)

func collect() {
	for range gcRequest { runtime.GC(); gcDone <- true }
}

//go:noinline
func sum(p *[5120]byte) int {
	n := 0
	for i := 0; i < len(p); i++ { n += int(p[i]) }
	return n
}

func recursive(n int) int {
	var data [5120]byte
	if sum(&data) != 0 { return -1 }
	data[0], data[len(data)-1] = byte(n), byte(n+1)
	want := int(byte(n)) + int(byte(n+1))
	if n == 0 {
		runtime.Gosched()
		return want
	}
	got := recursive(n-1)
	if sum(&data) != want { return -100 }
	return got + want
}

func argument(seed [16]int) int {
	runtime.Gosched()
	return seed[7]
}

func pointers(p *int) int {
	var values [4]*int
	if values[0] != nil { return -1 }
	values[0] = p
	runtime.Gosched()
	gcRequest <- true
	<-gcDone
	return *values[0]
}

// Check both chunk widths, including slots first reached after a shorter
// traversal returned a partially used chunk to the cache.
//go:noinline
func sumSmall(p *[1024]byte) int {
	n := 0
	for _, v := range p { n += int(v) }
	return n
}

func small(n int) int {
	var data [1024]byte
	if sumSmall(&data) != 0 { panic("small reused bytes") }
	data[0], data[len(data)-1] = byte(n), byte(n+1)
	want := int(byte(n)) + int(byte(n+1))
	got := 0
	if n == 0 { runtime.Gosched() } else { got = small(n-1) }
	if sumSmall(&data) != want { panic("small live bytes") }
	return got + want
}

//go:noinline
func pointerSum(p *[256]*int) int {
	n := 0
	for _, v := range p { if v != nil { n += *v } }
	return n
}

func pointerRecursive(n int) int {
	var values [256]*int
	if pointerSum(&values) != 0 { panic("reused pointers") }
	p := new(int)
	*p = n+1
	values[0], values[255] = p, p
	got := 0
	if n == 0 {
		runtime.Gosched()
		gcRequest <- true
		<-gcDone
	} else { got = pointerRecursive(n-1) }
	if pointerSum(&values) != 2*(n+1) { panic("live pointers") }
	return got + 2*(n+1)
}

func largeArgument(seed [1024]byte) int {
	runtime.Gosched()
	return int(seed[0]) + int(seed[1023])
}

// These keep factory initialization: their first array clear is outside the
// initial straight-line state, or its initializer performs other work.
func afterBoundary(n int) int {
	runtime.Gosched()
	var data [5120]byte
	if sum(&data) != 0 { panic("late zero") }
	data[0] = byte(n)
	runtime.Gosched()
	return sum(&data)
}

func inBranch(n int) int {
	if n >= 0 {
		var data [5120]byte
		if sum(&data) != 0 { panic("branch zero") }
		data[0] = byte(n)
		runtime.Gosched()
		return sum(&data)
	}
	return -1
}

var zeroCalls int
func zeroByte() byte { zeroCalls++; return 0 }
func effectful(n int) int {
	var data = [5120]byte{zeroByte()}
	if sum(&data) != 0 || zeroCalls != n { panic("effectful zero") }
	data[0] = byte(n)
	runtime.Gosched()
	return sum(&data)
}

func arrayResult(n int) (out [5120]byte) {
	runtime.Gosched()
	if sum(&out) != 0 { panic("array result zero") }
	out[0] = byte(n)
	return
}

func main() {
	go collect()
	for i := 0; i < 20; i++ {
		if recursive(64) != 4225 { panic("recursive result") }
		if argument([16]int{7: i}) != i { panic("array argument") }
		if pointers(&i) != i { panic("pointer result") }
		if afterBoundary(i) != i || inBranch(i) != i { panic("fallback local") }
		if effectful(i+1) != i+1 { panic("initializer effect") }
		result := arrayResult(i)
		if result[0] != byte(i) || result[5119] != 0 { panic("array result") }
	}
	for _, n := range []int{37, 38, 44, 49, 91, 17, 96, 38, 91} {
		if recursive(n) != (n+1)*(n+1) { panic("large tail result") }
		if small(n) != (n+1)*(n+1) { panic("small tail result") }
		if pointerRecursive(n) != (n+1)*(n+2) { panic("pointer tail result") }
		if largeArgument([1024]byte{0: byte(n), 1023: 7}) != n+7 { panic("large array argument") }
	}
	close(gcRequest)
	println("frame-zero-ok")
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
		{"nocoro/inline", "nocoro", ""},
		{"nocoro/noinline", "nocoro", "-l"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "frame-zero.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+test.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			assembly, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, assembly)
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOGC=1", "GOMAXPROCS=1")
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "frame-zero-ok" {
				t.Fatalf("execution = (%v, %q), want frame-zero-ok", err, out)
			}
			if test.experiment != "coro" {
				return
			}
			if !strings.Contains(string(assembly), "runtime.coroTakeSelfFrame(SB)") {
				t.Fatalf("recursive function did not select explicit self frames:\n%s", assembly)
			}
			if !strings.Contains(string(assembly), "main.pointers.coro STEXT") {
				t.Fatalf("pointer-array function was not lowered:\n%s", assembly)
			}
			match := regexp.MustCompile(`(?m)^main\.recursive\.coro STEXT .* locals=(0x[0-9a-f]+)`).FindSubmatch(assembly)
			if len(match) != 2 {
				t.Fatalf("missing recursive coroutine factory frame size:\n%s", assembly)
			}
			locals, err := strconv.ParseUint(string(match[1]), 0, 64)
			if err != nil {
				t.Fatal(err)
			}
			if locals >= 512 {
				t.Fatalf("factory native locals = %d bytes, want less than 512; zero array should be initialized in the typed frame", locals)
			}
			t.Logf("factory native locals = %d bytes", locals)
		})
	}
}
