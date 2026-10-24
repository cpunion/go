// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"encoding/binary"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestFrameHeaderPadding(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const descend = `
func FIRST(a uint16, b uint8, p *int, n int) int {
	if n == 0 {
		gcRequest <- true
		<-gcDone
		runtime.Gosched()
		return int(a) + int(b) + *p
	}
	child := SECOND(a, b, p, n-1)
	return child + int(a) + int(b) + *p
}

`
	program := `package main
import "runtime"
var gcRequest = make(chan bool)
var gcDone = make(chan bool)
func collect() { for range gcRequest { runtime.GC(); gcDone <- true } }
func plain(a uint16, b uint8, flag bool, p *int) int {
	runtime.Gosched()
	gcRequest <- true
	<-gcDone
	result := int(a) + int(b) + *p
	if flag { result++ }
	return result
}
func truth(n int, value bool) bool {
	if n == 0 { runtime.Gosched(); return value }
	child := truth(n-1, value)
	return child
}
func array(n int, value [3]byte, p *int) int {
	if n == 0 { runtime.Gosched(); return *p }
	child := array(n-1, value, p)
	return child + int(value[0]) + int(value[1]) + int(value[2])
}
func main() {
	go collect()
	for round := 0; round < 3; round++ {
		value := new(int)
		*value = 11 + round
		for _, depth := range []int{0, 1, 64, 256, 262, 263, 264, 267, 268, 4096} {
			want := 0x7501 + 0x81 + *value
			if plain(0x7501, 0x81, true, value) != want+1 { panic("plain padding") }
			if plain(0x7501, 0x81, false, value) != want { panic("plain reuse") }
			if self(0x7501, 0x81, value, depth) != (depth+1)*want { panic("self padding") }
			if first(0x7501, 0x81, value, depth) != (depth+1)*want { panic("mutual padding") }
			if !truth(depth, true) || truth(depth, false) { panic("packed result") }
			if array(depth, [3]byte{7, 0x83, 0xfe}, value) != *value + depth*(7+0x83+0xfe) { panic("packed array") }
		}
	}
	close(gcRequest)
	println("frame-header-padding-ok")
}
`
	for _, pair := range [][2]string{{"self", "self"}, {"first", "second"}, {"second", "first"}} {
		program += strings.NewReplacer("FIRST", pair[0], "SECOND", pair[1]).Replace(descend)
	}
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags string }{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"disabled", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "frame-padding.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, out)
				}
				for _, name := range []string{"plain", "self", "first", "second", "truth", "array"} {
					if !strings.Contains(string(out), "main."+name+".coro") {
						t.Fatalf("missing native factory %s", name)
					}
				}
			}
			for _, procs := range []string{"1", "4"} {
				cmd := testenv.Command(t, exe)
				cmd.Env = append(cmd.Environ(), "GOGC=1", "GOMAXPROCS="+procs)
				if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "frame-header-padding-ok" {
					t.Fatalf("P%s execution = (%v, %q)", procs, err, out)
				}
			}
		})
	}
}

func TestFrameStatePaddingFusedBoundary(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const descend = `
func FIRST(a uint16, b uint8, p *int, n int) int {
	var data [SIZE]byte
	data[0] = byte(n)
	if n == 0 { runtime.Gosched(); return int(a)+int(b)+*p+consume(&data) }
	child := SECOND(a,b,p,n-1)
	return child + int(a)+int(b)+*p+consume(&data)
}
`
	const main = `package main
import "runtime"
//go:noinline
func consume(data *[SIZE]byte) int { return int(data[0]) }
func main() {
	value := 11
	for _, depth := range []int{1,64,256,4096} {
		want := (depth+1)*(0x7501+0x81+value)
		for i:=0; i<=depth; i++ { want += int(byte(i)) }
		if first(0x7501,0x81,&value,depth) != want { panic("chunk boundary") }
	}
	println("chunk-boundary-ok")
}
`
	for _, test := range []struct{ array, frame int }{
		{432, 504}, // Header padding enables equal-cost typed chunks.
		{440, 520}, // Preserve an existing proof across the 512-byte cliff.
		{448, 520}, // Both layouts admit chunks in the same allocator class.
	} {
		t.Run(strconv.Itoa(test.array), func(t *testing.T) {
			program := main
			for _, pair := range [][2]string{{"first", "second"}, {"second", "first"}} {
				program += strings.NewReplacer("FIRST", pair[0], "SECOND", pair[1]).Replace(descend)
			}
			program = strings.ReplaceAll(program, "SIZE", strconv.Itoa(test.array))
			source := filepath.Join(t.TempDir(), "main.go")
			if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
				t.Fatal(err)
			}
			exe := filepath.Join(t.TempDir(), "boundary.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags=-l -d=coro=4 -S", "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
			assembly, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, assembly)
			}
			// The type descriptor starts with its byte size on both supported
			// little-endian native targets. Inspect the actual emitted frame,
			// so header packing preserves the selected allocation-boundary policy.
			lines := strings.Split(string(assembly), "\n")
			found := 0
			for i, line := range lines {
				if !strings.HasPrefix(line, "type:noalg.struct {") ||
					!strings.Contains(line, ".coroResume") ||
					!strings.Contains(line, "["+strconv.Itoa(test.array)+"]") {
					continue
				}
				found++
				if i+1 >= len(lines) {
					t.Fatal("missing frame type data")
				}
				words := strings.Fields(lines[i+1])
				if len(words) < 9 || words[0] != "0x0000" {
					t.Fatalf("invalid frame type data: %q", lines[i+1])
				}
				var size [8]byte
				for j := range size {
					value, err := strconv.ParseUint(words[j+1], 16, 8)
					if err != nil {
						t.Fatal(err)
					}
					size[j] = byte(value)
				}
				if got := binary.LittleEndian.Uint64(size[:]); got != uint64(test.frame) {
					t.Fatalf("frame size = %d, want %d to preserve chunk admission", got, test.frame)
				}
			}
			if found != 1 {
				t.Fatalf("found %d typed mutual frames, want one", found)
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOGC=1", "GOMAXPROCS=1")
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "chunk-boundary-ok" {
				t.Fatalf("execution = (%v, %q)", err, out)
			}
		})
	}
}

func TestFrameStatePaddingLayout(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"
func mixed(a bool, p *int, b bool, q *int, c bool, n int) int {
	runtime.Gosched()
	result := *p + *q + n
	if a { result++ }
	if b { result += 2 }
	if c { result += 4 }
	return result
}
func main() {
	value := 5
	if mixed(true,&value,true,&value,true,3) != 20 { panic("state padding") }
	println("state-padding-ok")
}
`
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "layout.exe")
	cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags=-l -d=coro=4 -S", "-o", exe, source)
	cmd.Env = append(cmd.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
	assembly, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, assembly)
	}
	lines := strings.Split(string(assembly), "\n")
	found := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "type:noalg.struct { F0 uint32;") ||
			!strings.Contains(line, "bool") || !strings.Contains(line, "*int") {
			continue
		}
		found++
		if i+1 >= len(lines) {
			t.Fatal("missing type data")
		}
		words := strings.Fields(lines[i+1])
		if len(words) < 17 || words[0] != "0x0000" {
			t.Fatalf("invalid type data: %q", lines[i+1])
		}
		var data [16]byte
		for j := range data {
			value, err := strconv.ParseUint(words[j+1], 16, 8)
			if err != nil {
				t.Fatal(err)
			}
			data[j] = byte(value)
		}
		if size, pointers := binary.LittleEndian.Uint64(data[:8]), binary.LittleEndian.Uint64(data[8:]); size != 48 || pointers != 40 {
			t.Fatalf("frame size/pointer prefix = %d/%d, want 48/40", size, pointers)
		}
	}
	if found != 1 {
		t.Fatalf("found %d small frame descriptors, want one", found)
	}
	cmd = testenv.Command(t, exe)
	cmd.Env = append(cmd.Environ(), "GOGC=1", "GOMAXPROCS=1")
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "state-padding-ok" {
		t.Fatalf("execution = (%v,%q)", err, out)
	}
}
