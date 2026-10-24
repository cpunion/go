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
	child := SECOND(n-1, kind)
	after++
	return child + *p
}
`
	program := `package main
import "runtime"
var gcRequest = make(chan bool)
var gcDone = make(chan bool)
var after int
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
				got, recovered := invoke(depth, kind)
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
					for _, helper := range []string{"coroRequestFusedFrame", "coroTakeFusedFrame", "coroAwaitFusedFrame", "coroCompleteFusedFrame"} {
						if !strings.Contains(string(out), "runtime."+helper+"(SB)") {
							t.Fatalf("%s missing %s:\n%s", name, helper, out)
						}
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
