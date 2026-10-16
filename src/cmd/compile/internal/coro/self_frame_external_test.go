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

func TestSelfFrameTerminalEntry(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" ||
		runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const program = `package main
import "runtime"

var gcRequest = make(chan bool)
var gcDone = make(chan bool)
var after int

func collect() {
	for range gcRequest { runtime.GC(); gcDone <- true }
}

func descend(n, kind int) int {
	p := new(int)
	*p = n
	if n == 0 {
		gcRequest <- true
		<-gcDone
		runtime.Gosched()
		if kind == 1 { var missing *int; return *missing }
		if kind == 2 { return 1 / (n - n) }
		return *p
	}
	child := descend(n-1, kind)
	after++
	return child + *p
}

func invoke(n, kind int) (result int, recovered any) {
	defer func() { recovered = recover() }()
	result = descend(n, kind)
	return
}

func pointer(n int, value *int) *int {
	if n == 0 {
		gcRequest <- true
		<-gcDone
		runtime.Gosched()
		return value
	}
	result := pointer(n-1, value)
	return result
}

func exiting(n int) {
	if n == 0 {
		runtime.Gosched()
		runtime.Goexit()
	}
	exiting(n-1)
	after++
}

func main() {
	go collect()
	for round := 0; round < 2; round++ {
		for _, depth := range []int{0, 1, 64, 256, 262, 263, 264, 4096} {
			value := new(int)
			*value = depth
			result := pointer(depth, value)
			runtime.GC()
			if result != value || *result != depth { panic("self-frame pointer result") }
			for kind := 0; kind < 3; kind++ {
				after = 0
				got, recovered := invoke(depth, kind)
				if kind == 0 {
					if recovered != nil || got != depth*(depth+1)/2 || after != depth {
						panic("normal self-frame completion")
					}
				} else if recovered == nil || after != 0 {
					panic("self-frame terminal continuation")
				}
			}
		}
	}
	for _, depth := range []int{1, 4096} {
		after = 0
		done := make(chan bool)
		go func() { defer close(done); exiting(depth); after++ }()
		<-done
		if after != 0 { panic("Goexit continuation") }
	}
	close(gcRequest)
	println("self-frame-terminal-ok")
}
`
	source := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(source, []byte(program), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, experiment, flags string }{
		{"coro/inline", "coro", "-d=coro=4"},
		{"coro/noinline", "coro", "-l -d=coro=4"},
		{"coro/checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"nocoro", "nocoro", "-l"},
	} {
		t.Run(test.name, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "self-frame.exe")
			cmd := testenv.Command(t, testenv.GoToolPath(t), "build",
				"-gcflags="+test.flags, "-o", exe, source)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+test.experiment, "GOWORK=off")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build failed: %v\n%s", err, out)
			}
			cmd = testenv.Command(t, exe)
			cmd.Env = append(cmd.Environ(), "GOGC=1", "GOMAXPROCS=1")
			if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "self-frame-terminal-ok" {
				t.Fatalf("execution = (%v, %q), want self-frame-terminal-ok", err, out)
			}
			if test.experiment != "coro" {
				return
			}
			for _, name := range []string{"descend", "pointer"} {
				cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "objdump",
					"-s", `^main\.`+name+`\.coro(\.func[0-9]+)?$`, exe)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("objdump failed: %v\n%s", err, out)
				}
				assembly := string(out)
				for _, helper := range []string{
					"runtime.coroTakeSelfFrame(SB)",
					"runtime.coroAwaitSelfFrame(SB)",
					"runtime.coroCompleteSelfFrame(SB)",
				} {
					if !strings.Contains(assembly, helper) {
						t.Fatalf("missing %s; self-frame path was not selected:\n%s", helper, assembly)
					}
				}
				if strings.Contains(assembly, "runtime.coroContinueSelfFrame(SB)") {
					t.Fatalf("local self-frame switch uses a separate continuation call:\n%s", assembly)
				}
				if got := strings.Count(assembly, "runtime.coroTerminalAction(SB)"); got != 1 {
					t.Fatalf("self-frame terminal queries = %d, want one at resume entry:\n%s", got, assembly)
				}
			}
		})
	}
}
