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

func TestCapturedDeferEnvironmentFallback(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
type box struct{n int;pad [2048]byte}
//go:noinline
func maker(p *box,seed int)func(int)int{return func(v int)int{if p.pad[2047]!=7{panic("lost environment")};p.n+=seed+v;return p.n}}
//go:noinline
func arguments()(n int){b:=&box{n:1};b.pad[2047]=7;f:=maker(b,2);defer func(){n=b.n}();defer f(1);f=nil;runtime.Gosched();b.n+=10;return}
//go:noinline
func noargs()(n int){b:=&box{n:1};f:=func()int{b.n++;return b.n};defer func(){n=b.n}();defer f();f=nil;runtime.Gosched();return}
//go:noinline
func one(y func(int)bool){y(1)}
//go:noinline
func ranged()(n int){for v:=range one{f:=func()int{n+=v;return n};defer f();f=nil};runtime.Gosched();return}
var eval int
//go:noinline
func argument()int{eval++;return 1}
//go:noinline
func nilCall(useNil bool)(caught any){defer func(){caught=recover()}();p:=&box{};p.pad[2047]=7;f:=maker(p,2);if useNil{f=nil};defer f(argument());runtime.Gosched();return}
var saved func(int)bool
var ready,release chan struct{}
var trace int
//go:noinline
func stash(y func(int)bool){saved=y;close(ready);<-release}
//go:noinline
func escaped(){for v:=range stash{runtime.LockOSThread();f:=func(){trace=10*trace+v};defer f();f=nil;runtime.UnlockOSThread()}}
func main(){
 if arguments()!=14||noargs()!=2||ranged()!=1{panic("captured defer environment")}
 before:=eval;if _,ok:=nilCall(true).(runtime.Error);!ok||eval!=before+1{panic("nil timing")};if nilCall(false)!=nil{panic("non-nil function")}
 ready=make(chan struct{});release=make(chan struct{});done:=make(chan struct{});go func(){escaped();close(done)}();<-ready;runtime.LockOSThread();saved(1);saved(2);runtime.UnlockOSThread();runtime.GC();close(release);<-done;if trace!=21{panic("ordinary callback environment")}
 runtime.LockOSThread();if arguments()!=14||noargs()!=2||ranged()!=1{panic("ordinary source control")};runtime.UnlockOSThread();println("captured-defer-fallback-ok")
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"native-inline", "coro", "-d=coro=4"},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2"},
		{"native-no-dwarf", "coro", "-d=coro=4 -dwarf=false -gendwarfinl=0"},
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
			if config.experiment == "coro" && strings.Contains(config.flags, "-l ") && !strings.Contains(string(out), "captured dynamic defer target requires an environment") {
				t.Fatal("captured callee entered dispatch without an environment contract")
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "captured-defer-fallback-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
