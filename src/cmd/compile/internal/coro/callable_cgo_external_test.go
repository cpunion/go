// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/buildcfg"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapturedCallableForeignConstruction(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveCGO(t)
	const source = `package main
/*
#cgo nocallback add
static int add(int n) { return n+10; }
*/
import "C"
func makeOne(seed int)func()int {_=C.add(C.int(seed));return func()int{return seed}}
func makeMany()[]func()int {
 var fs []func()int
 for i:=0;i<3;i++ {raw:=C.add(C.int(i));value:=int(raw);fs=append(fs,func()int{return value})}
 return fs
}
func main(){f:=makeOne(7);if f()!=7 {panic("captured foreign parameter")};fs:=makeMany();for i,f:=range fs {if f()!=i+10 {panic("captured foreign loop cell")}};println("cgo-capture-ok")}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "program")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", binary, path)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"makeOne", "makeMany"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission\n%s", name, out)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "cgo-capture-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
