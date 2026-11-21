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

func TestFusedAwaitLifecycle(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	// Identical typed frames permit compatible chunks across distinct resumes.
	// Each frame keeps a heap pointer live across its child and a full GC.
	const descend = `
func FIRST(n, kind int) int {
	p := new(int)
	*p = n
	if n == 0 {
		gcRequest <- true
		<-gcDone
		runtime.Gosched()
		if kind == 1 { var missing *int; return *missing }
		return *p
	}
	child := SECOND(operand(0, n-1), operand(1, kind))
	after++
	return child + *p
}
`
	program := `package main
import "runtime"
var gcRequest = make(chan bool)
var gcDone = make(chan bool)
var after int
var operands int
var wrongOrder bool
func operand(index, value int) int {
	if index != operands%2 { wrongOrder = true }
	operands++
	return value
}
func collect() { for range gcRequest { runtime.GC(); gcDone <- true } }
func invoke(n, kind int) (result int, recovered any) {
	defer func() { recovered = recover() }()
	result = first(n, kind)
	return
}
func main() {
	go collect()
	for round := 0; round < 2; round++ {
		for _, depth := range []int{0, 1, 64, 256, 262, 263, 264, 267, 268, 4096} {
			for kind := 0; kind < 2; kind++ {
				after = 0
				operands, wrongOrder = 0, false
				got, recovered := invoke(depth, kind)
				if wrongOrder || operands != 2*depth { panic("fused await operands") }
				if kind == 0 {
					if recovered != nil || got != depth*(depth+1)/2 || after != depth {
						panic("normal fused completion")
					}
				} else if recovered == nil || after != 0 { panic("fused panic continuation") }
			}
		}
	}
	close(gcRequest)
	println("fused-await-lifecycle-ok")
}
`
	program += strings.NewReplacer("FIRST", "first", "SECOND", "second").Replace(descend)
	program += strings.NewReplacer("FIRST", "second", "SECOND", "first").Replace(descend)
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
			exe := filepath.Join(t.TempDir(), "fused-await.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"first", "second"} {
					cmd := testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
						"-s", `^main\.`+name+`\.coro(\.func[0-9]+)?$`, exe)
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("objdump: %v\n%s", err, out)
					}
					for _, helper := range []string{
						"coroRequestFusedFrame", "coroTakeFusedFrame", "coroAwaitFusedFrame", "coroCompleteFusedFrame",
						"coroTakeFusedChunkBoundary", "coroAdvanceFusedChunk", "coroPopFusedChunk",
					} {
						if !strings.Contains(string(out), "runtime."+helper+"(SB)") {
							t.Fatalf("%s missing %s:\n%s", name, helper, out)
						}
					}
					if linkedCoroStackUnsafe(t, exe, "main."+name+".coro.stackgate") {
						t.Fatalf("%s exceeded the native stack budget", name)
					}
				}
			}
			for _, procs := range []string{"1", "4"} {
				cmd := testenv.Command(t, exe)
				cmd.Env = append(cmd.Environ(), "GOGC=1", "GOMAXPROCS="+procs)
				if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "fused-await-lifecycle-ok" {
					t.Fatalf("P%s execution = (%v, %q)", procs, err, out)
				}
			}
		})
	}
}

// A warmed private coroutine keeps its fused child visible across GC and
// source enumeration. Its later spawn publishes the scheduler to siblings.
func TestPrivateFusedAwaitReuse(t *testing.T) {
	const program = `package main

import (
	"bytes"
	"runtime"
	"sync/atomic"
)

var entered, observed atomic.Bool

//go:noinline
func cachedLeaf(value *int) int {
	runtime.Gosched()
	return *value
}

func worker(done chan int) {
	runtime.Gosched()
	done <- 7
}

//go:noinline
func privateRoot(done chan int) int {
	value := 9
	total := 0
	for i := 0; i < 32; i++ {
		total += cachedLeaf(&value)
	}
	entered.Store(true)
	for !observed.Load() {
		if cachedLeaf(&value) != 9 {
			panic("lost value across yield")
		}
	}

	// Start sharing the same scheduler after warming its fused holder cache.
	// Later awaits must still interleave with siblings.
	go worker(done)
	for i := 0; i < 32; i++ {
		total += cachedLeaf(&value)
	}
	return total + <-done
}

func observe(seen chan bool) {
	for !entered.Load() {
		runtime.Gosched()
	}
	buf := make([]byte, 64<<10)
	runtime.GC()
	found := false
	for i := 0; i < 32 && !found; i++ {
		n := runtime.Stack(buf, true)
		found = bytes.Contains(buf[:n], []byte("main.cachedLeaf(")) &&
			bytes.Contains(buf[:n], []byte("main.privateRoot("))
		runtime.Gosched()
	}
	observed.Store(true)
	seen <- found
}

func main() {
	// The observer must run as an ordinary G, independently of the root.
	var entry [2048]byte
	if runtime.Stack(entry[:], false) == 0 {
		panic("missing ordinary entry")
	}
	done, seen := make(chan int), make(chan bool, 1)
	go observe(seen)
	if privateRoot(done) != 64*9+7 || !<-seen {
		panic("fused frame reuse lost values or source frames")
	}
	println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, program, nil,
		[]string{"cachedLeaf", "privateRoot"}, "cachedLeaf", "privateRoot")
}
