// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSwitchReturn(t *testing.T) {
	const source = `package main
import "runtime"

type failure string
func (e failure) Error() string { return string(e) }
var errValue error = failure("switch")
var trace int
func record() { trace++ }

func switchUint(x int) uint64 {
	runtime.Gosched()
	switch x { case 0: return 1 << 40; default: return 7 }
}
func switchByte(x int) uint8 {
	runtime.Gosched()
	switch x { case 0: return 3; default: return 7 }
}
func switchError(x int) error {
	runtime.Gosched()
	switch x { case 0: return errValue; default: return nil }
}
func switchBare(x bool) (result uint64) {
	runtime.Gosched()
	result = 19
	switch x { case true: return }
	return 23
}
func switchVoid(x bool) {
	defer record()
	runtime.Gosched()
	switch x { case true: return }
	trace += 10
}
func switchType(v any) uint64 {
	runtime.Gosched()
	switch x := v.(type) { case uint64: return x; default: return 11 }
}
func switchAssignment(x int) uint64 {
	runtime.Gosched()
	y := uint64(41)
	switch x { case 0: y = 42; default: y = 43 }
	return y
}
func switchClosure(x bool) func() uint64 {
	runtime.Gosched()
	var f func() uint64
	switch x {
	case true: f = func() uint64 { return 99 }
	default: f = func() uint64 { return 100 }
	}
	return f
}
func switchBreak(x int) int {
	runtime.Gosched()
	y := 0
	for i := 0; i < 2; i++ {
		switch x {
		case 0:
			y += 1
			goto local
			y += 100000
		local:
			break
		case 1:
			switch i { case 0: y += 10; break; default: y += 20 }
			for j := 0; j < 3; j++ { y += 100; if j == 1 { break } }
			if i == 0 { break }
			y += 1000
		default:
			y += 10000
		}
		y += 2
	}
	return y
}
func main() {
	if switchUint(0) != 1<<40 || switchUint(1) != 7 { panic("uint64 return") }
	if switchByte(0) != 3 || switchByte(1) != 7 { panic("uint8 return") }
	if switchError(0) != errValue || switchError(1) != nil { panic("error return") }
	if switchBare(true) != 19 || switchBare(false) != 23 { panic("bare return") }
	switchVoid(true)
	if trace != 1 { panic("void return cleanup") }
	switchVoid(false)
	if trace != 12 { panic("void fallthrough cleanup") }
	if switchType(uint64(1<<40)) != 1<<40 || switchType(0) != 11 { panic("type switch") }
	if switchAssignment(0) != 42 || switchAssignment(1) != 43 { panic("assignment switch") }
	if switchClosure(true)() != 99 || switchClosure(false)() != 100 { panic("closure return") }
	if switchBreak(0) != 6 || switchBreak(1) != 1434 || switchBreak(2) != 20004 { panic("local switch break") }
	println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]string{"switchUint", "switchByte", "switchError", "switchBare", "switchVoid", "switchType"},
		[]string{"switchAssignment", "switchClosure", "switchBreak"})
}

func TestSwitchReturnCgo(t *testing.T) {
	testenv.MustHaveCGO(t)
	const source = `package main
/*
#cgo nocallback add
static unsigned long long add(unsigned long long n) { return n + 10; }
*/
import "C"

//go:noinline
func directSwitch(x int) uint64 {
	value := C.add(C.ulonglong(x))
	n := uint64(value)
	switch x { case 0: return n; default: return n + 1 }
}
func directIf(x int) uint64 {
	value := C.add(C.ulonglong(x))
	n := uint64(value)
	if x == 0 { return n }
	return n + 1
}
func main() {
	if directSwitch(0) != 10 || directSwitch(1) != 12 { panic("direct C structured return") }
	if directIf(0) != 10 || directIf(1) != 12 { panic("direct C if return") }
	println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, []string{"directSwitch"}, []string{"directIf"})
}

func testSwitchReturnProgram(t *testing.T, source string, fallback, lowered []string) {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	file := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(file, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, experiment, flags string
		checkLowering           bool
	}{
		{"coro/inline", "coro", "-d=coro=4", false},
		{"coro/noinline", "coro", "-l -d=coro=4", true},
		{"nocoro/inline", "nocoro", "", false},
		{"nocoro/noinline", "nocoro", "-l", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "switchreturn.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+test.flags, "-o", exe, file)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			data, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build failed: %v\n%s", err, data)
			}
			buildOutput := string(data)
			if test.checkLowering {
				for _, name := range fallback {
					want := "coro: skip main." + name + ": return in unsupported control flow switch"
					if !strings.Contains(string(data), want) {
						t.Errorf("missing diagnostic %q\n%s", want, data)
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd = testenv.CommandContext(t, ctx, exe)
			data, err = cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(data)) != "switch-return-ok" {
				t.Fatalf("execution = (%v, %q), want switch-return-ok", err, data)
			}
			if !test.checkLowering {
				return
			}
			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			data, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("nm failed: %v\n%s", err, data)
			}
			for _, name := range fallback {
				if strings.Contains(string(data), "main."+name+".coro") {
					t.Errorf("unsupported return has a coroutine factory: %s", name)
				}
			}
			for _, name := range lowered {
				if !strings.Contains(string(data), "main."+name+".coro") {
					t.Errorf("supported switch lost its coroutine factory: %s\n%s", name, buildOutput)
				}
			}
		})
	}
}
