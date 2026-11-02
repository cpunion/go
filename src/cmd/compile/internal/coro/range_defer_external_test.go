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
func empty(y func(int)bool){}
//go:noinline
func markerOnly(repeated bool){if repeated{for i:=0;i<3;i++{for v:=range empty{defer add(v)}}}else{for v:=range empty{defer add(v)}}}
//go:noinline
func repeatedMarkers(){for i:=0;i<4;i++{for v:=range one{defer add(i*10+v)}}}
//go:noinline
func sumArray(p *[256]int)int{sum:=0;for _,v:=range p{sum+=v};return sum}
//go:noinline
func bigStackTarget(){var values [256]int;for i:=range values{values[i]=i};add(sumArray(&values))}
//go:noinline
func sourceBudgetOwner(){for range one{defer bigStackTarget()}}
//go:noinline
func overBudgetOwner(){for range one{defer deepStackTarget()}}
//go:noinline
func deepStackTarget(){add(stack0(0))}
//go:noinline
func stack0(v int)int{return stack1(v)+1}
//go:noinline
func stack1(v int)int{return stack2(v)+1}
//go:noinline
func stack2(v int)int{return stack3(v)+1}
//go:noinline
func stack3(v int)int{return stack4(v)+1}
//go:noinline
func stack4(v int)int{return stack5(v)+1}
//go:noinline
func stack5(v int)int{return stack6(v)+1}
//go:noinline
func stack6(v int)int{return stack7(v)+1}
//go:noinline
func stack7(v int)int{return stack8(v)+1}
//go:noinline
func stack8(v int)int{return stack9(v)+1}
//go:noinline
func stack9(v int)int{return stack10(v)+1}
//go:noinline
func stack10(v int)int{return stack11(v)+1}
//go:noinline
func stack11(v int)int{return stack12(v)+1}
//go:noinline
func stack12(v int)int{return stack13(v)+1}
//go:noinline
func stack13(v int)int{return stack14(v)+1}
//go:noinline
func stack14(v int)int{return stack15(v)+1}
//go:noinline
func stack15(v int)int{return stack16(v)+1}
//go:noinline
func stack16(v int)int{return stack17(v)+1}
//go:noinline
func stack17(v int)int{return stack18(v)+1}
//go:noinline
func stack18(v int)int{return stack19(v)+1}
//go:noinline
func stack19(v int)int{return stack20(v)+1}
//go:noinline
func stack20(v int)int{return stack21(v)+1}
//go:noinline
func stack21(v int)int{return stack22(v)+1}
//go:noinline
func stack22(v int)int{return stack23(v)+1}
//go:noinline
func stack23(v int)int{return stack24(v)+1}
//go:noinline
func stack24(v int)int{return stack25(v)+1}
//go:noinline
func stack25(v int)int{return stack26(v)+1}
//go:noinline
func stack26(v int)int{return stack27(v)+1}
//go:noinline
func stack27(v int)int{return stack28(v)+1}
//go:noinline
func stack28(v int)int{return stack29(v)+1}
//go:noinline
func stack29(v int)int{return stack30(v)+1}
//go:noinline
func stack30(v int)int{return stack31(v)+1}
//go:noinline
func stack31(v int)int{return stack32(v)+1}
//go:noinline
func stack32(v int)int{return stack33(v)+1}
//go:noinline
func stack33(v int)int{return stack34(v)+1}
//go:noinline
func stack34(v int)int{return stack35(v)+1}
//go:noinline
func stack35(v int)int{return stack36(v)+1}
//go:noinline
func stack36(v int)int{return stack37(v)+1}
//go:noinline
func stack37(v int)int{return stack38(v)+1}
//go:noinline
func stack38(v int)int{return stack39(v)+1}
//go:noinline
func stack39(v int)int{return stack40(v)+1}
//go:noinline
func stack40(v int)int{return stack41(v)+1}
//go:noinline
func stack41(v int)int{return stack42(v)+1}
//go:noinline
func stack42(v int)int{return stack43(v)+1}
//go:noinline
func stack43(v int)int{return stack44(v)+1}
//go:noinline
func stack44(v int)int{return stack45(v)+1}
//go:noinline
func stack45(v int)int{return stack46(v)+1}
//go:noinline
func stack46(v int)int{return stack47(v)+1}
//go:noinline
func stack47(v int)int{return stack48(v)+1}
//go:noinline
func stack48(v int)int{return stack49(v)+1}
//go:noinline
func stack49(v int)int{return stack50(v)+1}
//go:noinline
func stack50(v int)int{return stack51(v)+1}
//go:noinline
func stack51(v int)int{return stack52(v)+1}
//go:noinline
func stack52(v int)int{return stack53(v)+1}
//go:noinline
func stack53(v int)int{return stack54(v)+1}
//go:noinline
func stack54(v int)int{return stack55(v)+1}
//go:noinline
func stack55(v int)int{return stack56(v)+1}
//go:noinline
func stack56(v int)int{return stack57(v)+1}
//go:noinline
func stack57(v int)int{return stack58(v)+1}
//go:noinline
func stack58(v int)int{return stack59(v)+1}
//go:noinline
func stack59(v int)int{return stack60(v)+1}
//go:noinline
func stack60(v int)int{return stack61(v)+1}
//go:noinline
func stack61(v int)int{return stack62(v)+1}
//go:noinline
func stack62(v int)int{return stack63(v)+1}
//go:noinline
func stack63(v int)int{return stack64(v)+1}
//go:noinline
func stack64(v int)int{return v}
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
func nilCall(useNil bool)(caught any){defer func(){caught=recover()}();for range one{f:=add;if useNil{f=nil};defer add(1);defer f(effect())};return nil}
//go:noinline
func unknownNil()(caught any){defer func(){caught=recover()}();var f func(int);for range one{defer add(1);defer f(effect())};return nil}
//go:noinline
func effect()int{add(4);return 0}
//go:noinline
func exiting(done chan struct{}){defer func(){close(done)}();for range one{defer add(1);defer runtime.Goexit()};defer add(3);add(0)}
//go:noinline
func rangeRecover()(caught any){for range one{defer func(){caught=recover();add(2)}()};panic("range")}
//go:noinline
func catchNamed(result *any){*result=recover();add(6)}
//go:noinline
func namedRangeRecover()(caught any){for range one{defer catchNamed(&caught)};panic("named")}
//go:noinline
func panicTarget(){panic("target")}
//go:noinline
func rangeFactory()(caught any){defer func(){caught=recover();add(9)}();for range one{defer add(1);defer panicTarget()};panic("body")}
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
//go:noinline
func ordinaryBodyOwner(){defer add(9);for v:=range stash{runtime.LockOSThread();defer add(v);runtime.UnlockOSThread()};defer add(8)}
func mixed(lockedOwner,ordinaryBody bool){trace=nil;ready=make(chan struct{});release=make(chan struct{});done:=make(chan struct{});go func(){if lockedOwner{runtime.LockOSThread();defer runtime.UnlockOSThread()};if ordinaryBody{ordinaryBodyOwner()}else{escapedOwner()};close(done)}();<-ready;if !lockedOwner{runtime.LockOSThread()};if !saved(1)||!saved(2){panic("yield stopped")};if !lockedOwner{runtime.UnlockOSThread()};runtime.GC();close(release);<-done;check(8,2,1,9)}
func check(want ...int){if !slices.Equal(trace,want){panic("range cleanup order")};trace=nil}
func main(){
 markerOnly(false);markerOnly(true);check();repeatedMarkers();check(31,21,11,1);overBudgetOwner();check(64);sourceBudgetOwner();check(32640)
 basic();check(8,3,2,1,9);separate();check(7,2,8,1,9);nested();check(8,2,9)
 if early()!=2{panic("early result")};check(2,1,9);recursive(1);check(1,9,2,10)
 if replacement()!="replacement"{panic("replacement recover")};check(1,9)
 if rangeRecover()!="range"{panic("range direct recover")};check(2)
 if namedRangeRecover()!="named"{panic("range named recover")};check(6)
 if rangeFactory()!="target"{panic("range terminal factory")};check(1,9)
 if _,ok:=nilCall(true).(runtime.Error);!ok{panic("nil deferred call")};check(4,1)
 if _,ok:=unknownNil().(runtime.Error);!ok{panic("unknown nil deferred call")};check(4,1)
 done:=make(chan struct{});go exiting(done);<-done;check(0,3,1)
 a,b:=make(chan int),make(chan int);done=make(chan struct{});go func(){held(a,b);close(done)}();for i:=1;i<=3;i++{if <-a!=i{panic("GC handshake")};runtime.GC();b<-1};<-done;check(3,2,1)
 mixed(false,false);mixed(true,false);mixed(false,true);mixed(true,true);runtime.LockOSThread();basic();runtime.UnlockOSThread();check(8,3,2,1,9);println("enclosing-range-defer-ok")
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
				for _, name := range []string{"basic", "separate", "nested", "early", "recursive", "replacement", "rangeRecover", "namedRangeRecover", "rangeFactory", "nilCall", "exiting", "held", "escapedOwner", "ordinaryBodyOwner", "markerOnly", "repeatedMarkers"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
							t.Fatal("lost native enclosing cleanup admission")
						}
					})
				}
				t.Run("over-budget", func(t *testing.T) {
					if !linkedCoroStackUnsafe(t, binary, "main.overBudgetOwner.coro.stackgate") {
						t.Fatal("enclosing owner ignored its deferred target's machine stack budget")
					}
				})
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
