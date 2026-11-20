// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSourcePublicRootQuickCompletion(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native coroutine executor is not supported")
	}
	// A private root can complete without the ordinary locked queue path.
	// Its public record must be removed before the scheduler is cleared/reused.
	const program = `package main
import("fmt";"runtime";"strings")
//go:noinline
func quick(n int)int {runtime.Gosched();return n+1}
var entry=quick
func main(){goto body;body:
 baseline:=runtime.NumGoroutine()
 for i:=0;i<100;i++{if entry(i)!=i+1{panic("quick result")}}
 runtime.GC()
 records:=make([]runtime.StackRecord,64);n,ok:=runtime.GoroutineProfile(records)
 if !ok||n!=baseline{panic(fmt.Sprintf("quick profile count %d want %d",n,baseline))}
 buf:=make([]byte,1<<20);n=runtime.Stack(buf,true)
 if strings.Contains(string(buf[:n]),"main.quick("){panic("completed public root remains visible")}
 if runtime.NumGoroutine()!=baseline{panic("quick count changed")}
 fmt.Println("public-root-quick-ok")
}
`
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"ordinary", "nocoro", "-l", ""},
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"lockrank", "coro,staticlockranking", "-l -d=coro=4", ""},
		{"ordinary-race", "nocoro", "-l", "-race"},
		{"coro-race", "coro", "-l -d=coro=4", "-race"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(dir, config.name)
			args := []string{"build", "-gcflags=" + config.flags, "-o", exe}
			if config.extra != "" {
				args = append(args, config.extra)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			native := config.experiment != "nocoro" && config.extra == ""
			if native && linkedCoroStackUnsafe(t, exe, "main.quick.coro.stackgate") {
				t.Fatal("quick lost native admission")
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=0,profstackdepth=128")
				data, err := run.CombinedOutput()
				if err != nil || string(data) != "public-root-quick-ok\n" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
