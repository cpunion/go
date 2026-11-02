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

func TestCapturedOperandCallbacks(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
type box struct{n int; pad [2048]byte}
//go:noinline
func twice(n int)int{return 2*n}
//go:noinline
func triple(n int)int{return 3*n}
//go:noinline
func maker(p *box,seed int)func(func(int)int)(int,*box){return func(cb func(int)int)(int,*box){p.n=cb(p.n)+seed;return p.n,p}}
//go:noinline
func deferOne()(n int){b:=&box{n:7};f:=maker(b,1);defer func(){n=b.n}();defer f(twice);f=nil;runtime.Gosched();return}
//go:noinline
func variants()(n int){b:=&box{n:7};f:=maker(b,1);defer func(){n=b.n}();defer f(twice);defer f(triple);runtime.Gosched();return}
//go:noinline
func union(useTriple bool)(n int){b:=&box{n:7};f:=maker(b,1);cb:=twice;if useTriple{cb=triple};defer func(){n=b.n}();defer f(cb);cb=nil;runtime.Gosched();return}
//go:noinline
func makeCallback(delta int)func(int)int{return func(n int)int{return n+delta}}
//go:noinline
func capturedCallback()(n int){b:=&box{n:7};f:=maker(b,2);cb:=makeCallback(3);defer func(){n=b.n}();defer f(cb);cb=makeCallback(1000);runtime.Gosched();return}
var evaluations int
//go:noinline
func argument()func(int)int{evaluations++;return twice}
//go:noinline
func nilCallee(useNil bool)(caught any){defer func(){caught=recover()}();f:=maker(&box{n:7},1);if useNil{f=nil};defer f(argument());runtime.Gosched();return}
//go:noinline
func nilCallback(useNil bool)(caught any){defer func(){caught=recover()}();f:=maker(&box{n:7},1);cb:=twice;if useNil{cb=nil};defer f(cb);runtime.Gosched();return}
var ready chan struct{}
var release chan struct{}
//go:noinline
func delayed(n int)int{ready<-struct{}{};<-release;return 2*n}
//go:noinline
func asyncMaker(p *box,seed int,done chan int)func(func(int)int)int{return func(cb func(int)int)int{p.n=cb(p.n)+seed;if p.pad[2047]!=byte(seed){panic("capture root")};done<-p.n;return p.n}}
//go:noinline
func spawn(p *box,seed int,done chan int){f:=asyncMaker(p,seed,done);go f(delayed);f=nil}
var observed any
//go:noinline
func recoverCallback()any{return recover()}
//go:noinline
func recoverMaker(p *any)func(func()any){return func(cb func()any){*p=recover();observed=cb()}}
//go:noinline
func recoverOwner()(caught any){f:=recoverMaker(&caught);defer f(recoverCallback);runtime.Gosched();panic("direct")}
//go:noinline
func panicCallback(){panic("replacement")}
//go:noinline
func terminalMaker(p *int)func(func()){return func(cb func()){*p=8;cb()}}
//go:noinline
func replacement()(caught any){defer func(){caught=recover()}();p:=new(int);f:=terminalMaker(p);defer f(panicCallback);runtime.Gosched();panic("original")}
//go:noinline
func exitCallback(){runtime.Goexit()}
var exited int
//go:noinline
func exiting(done chan struct{}){defer func(){close(done)}();f:=terminalMaker(&exited);defer f(exitCallback);runtime.Gosched();panic("suppressed")}
//go:noinline
func one(y func(int)bool){y(7)}
//go:noinline
func rangeOwner()(n int){b:=&box{};defer func(){n=b.n}();for v:=range one{b.n=v;f:=maker(b,1);defer f(twice);f=nil};runtime.Gosched();return}
var saved func(int)bool
var callbackReady,callbackRelease chan struct{}
//go:noinline
func stash(y func(int)bool){saved=y;close(callbackReady);<-callbackRelease}
//go:noinline
func escapedOwner()(n int){b:=&box{};defer func(){n=b.n}();for v:=range stash{runtime.LockOSThread();b.n+=v;f:=maker(b,1);defer f(twice);f=nil;runtime.UnlockOSThread()};return}
func mixed(locked bool){callbackReady=make(chan struct{});callbackRelease=make(chan struct{});done:=make(chan int,1);go func(){if locked{runtime.LockOSThread();defer runtime.UnlockOSThread()};done<-escapedOwner()}();<-callbackReady;if !locked{runtime.LockOSThread()};saved(1);saved(2);if !locked{runtime.UnlockOSThread()};runtime.GC();close(callbackRelease);if <-done!=15{panic("escaped callback cells")}}
func controls(){if deferOne()!=15||variants()!=45||union(false)!=15||union(true)!=22||capturedCallback()!=12||rangeOwner()!=15{panic("callback operands")};before:=evaluations;if _,ok:=nilCallee(true).(runtime.Error);!ok||evaluations!=before+1{panic("callee nil timing")};if nilCallee(false)!=nil{panic("non-nil callee")};if _,ok:=nilCallback(true).(runtime.Error);!ok||nilCallback(false)!=nil{panic("callback nil timing")};observed="unset";if recoverOwner()!="direct"||observed!=nil||replacement()!="replacement"{panic("callback terminal ownership")}}
func main(){controls();runtime.LockOSThread();controls();runtime.UnlockOSThread();d:=make(chan struct{});go exiting(d);<-d;if exited!=8{panic("callback Goexit")};ready=make(chan struct{},32);release=make(chan struct{});done:=make(chan int,32);for i:=0;i<32;i++{p:=&box{n:10};p.pad[2047]=byte(i);spawn(p,i,done)};for i:=0;i<32;i++{<-ready};runtime.GC();close(release);sum:=0;for i:=0;i<32;i++{sum+=<-done};if sum!=1136{panic("callback parked captures")};mixed(false);mixed(true);println("captured-operand-callbacks-ok")}
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
				for _, name := range []string{"deferOne", "variants", "union", "capturedCallback", "nilCallee", "nilCallback", "spawn", "recoverOwner", "replacement", "exiting", "rangeOwner", "escapedOwner"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
							t.Fatal("captured operand callback lost native admission")
						}
					})
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "captured-operand-callbacks-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
