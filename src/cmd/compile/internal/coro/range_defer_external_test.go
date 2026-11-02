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

func TestEnclosingRangeDefer(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import("runtime";"slices")
var trace []int
//go:noinline
func add(v int){trace=append(trace,v)}
//go:noinline
func three(y func(int)bool){for i:=1;i<=3;i++{if !y(i){return}}}
//go:noinline
func one(y func(int)bool){y(1)}
//go:noinline
func basic(){defer add(9);for v:=range three{defer add(v);runtime.Gosched()};defer add(8)}
//go:noinline
func separate(){defer add(9);for v:=range one{defer add(v)};defer add(8);for v:=range one{defer add(v+1)};defer add(7)}
//go:noinline
func nested(){defer add(9);for i:=range one{for j:=range one{defer add(i+j)}};defer add(8)}
//go:noinline
func early()int{defer add(9);for v:=range three{defer add(v);if v==2{return v}};return -1}
//go:noinline
func recursive(n int){defer add(9+n);for v:=range one{defer add(v+n);if n>0{recursive(n-1)}}}
//go:noinline
func replacement()(caught any){defer func(){caught=recover();add(9)}();for range one{defer add(1);defer func(){panic("replacement")}()};panic("original")}
//go:noinline
func nilCall(useNil bool)(caught any){defer func(){caught=recover()}();f:=add;if useNil{f=nil};for range one{defer add(1);defer f(effect())};return nil}
//go:noinline
func unknownNil()(caught any){defer func(){caught=recover()}();var f func(int);for range one{defer add(1);defer f(effect())};return nil}
//go:noinline
func effect()int{add(4);return 0}
//go:noinline
func exiting(done chan struct{}){defer close(done);for range one{defer add(1);defer runtime.Goexit()};defer add(3);add(0)}
type object struct{value int;next *object;pad [2048]byte}
//go:noinline
func objects(y func(*object)bool){for i:=1;i<=3;i++{p:=&object{value:i,next:&object{value:i+10}};p.pad[2047]=byte(i);if !y(p){return}}}
//go:noinline
func held(ready,release chan int){for p:=range objects{defer func(){if p.pad[2047]!=byte(p.value)||p.next.value!=p.value+10{panic("lost range defer root")};add(p.value)}();ready<-p.value;<-release}}
var saved func(int)bool
var ready,release chan struct{}
//go:noinline
func stash(y func(int)bool){saved=y;close(ready);<-release}
//go:noinline
func escapedOwner(){defer add(9);for v:=range stash{defer add(v)};defer add(8)}
func mixed(lockedOwner bool){trace=nil;ready=make(chan struct{});release=make(chan struct{});done:=make(chan struct{});go func(){if lockedOwner{runtime.LockOSThread();defer runtime.UnlockOSThread()};escapedOwner();close(done)}();<-ready;if !lockedOwner{runtime.LockOSThread()};if !saved(1)||!saved(2){panic("yield stopped")};if !lockedOwner{runtime.UnlockOSThread()};runtime.GC();close(release);<-done;check(8,2,1,9)}
func check(want ...int){if !slices.Equal(trace,want){panic("range cleanup order")};trace=nil}
func main(){
 basic();check(8,3,2,1,9);separate();check(7,2,8,1,9);nested();check(8,2,9)
 if early()!=2{panic("early result")};check(2,1,9);recursive(1);check(1,9,2,10)
 if replacement()!="replacement"{panic("replacement recover")};check(1,9)
 if _,ok:=nilCall(true).(runtime.Error);!ok{panic("nil deferred call")};check(4,1)
 if _,ok:=unknownNil().(runtime.Error);!ok{panic("unknown nil deferred call")};check(4,1)
 done:=make(chan struct{});go exiting(done);<-done;check(0,3,1)
 a,b:=make(chan int),make(chan int);done=make(chan struct{});go func(){held(a,b);close(done)}();for i:=1;i<=3;i++{if <-a!=i{panic("GC handshake")};runtime.GC();b<-1};<-done;check(3,2,1)
 mixed(false);mixed(true);runtime.LockOSThread();basic();runtime.UnlockOSThread();check(8,3,2,1,9);println("enclosing-range-defer-ok")
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
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"basic", "separate", "nested", "early", "recursive", "replacement", "nilCall", "exiting", "held", "escapedOwner"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native enclosing cleanup admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "enclosing-range-defer-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
