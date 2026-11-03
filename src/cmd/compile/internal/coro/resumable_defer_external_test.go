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

func TestResumableDeferCalls(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
var evaluated int
func argument(n int)int{evaluated++;return n}
//go:noinline
func publicRecover(out *any){runtime.Gosched();*out=recover()}
//go:noinline
func asyncRecover(out *any,n *int,v int){runtime.Gosched();*n+=v;*out=recover()}
//go:noinline
func otherRecover(out *any,n *int,v int){runtime.Gosched();*n+=2*v;*out=recover()}
//go:noinline
func plainRecover(out *any,n *int,v int){*n+=3*v;*out=recover()}
//go:noinline
func record(out *[]int,v int){runtime.Gosched();*out=append(*out,v)}
//go:noinline
func asyncPanic(){runtime.Gosched();panic("cleanup")}
//go:noinline
func onlyDefer()(n int){defer func(){runtime.Gosched();n=7}();return}
//go:noinline
func literal()(out any,n int){defer func(){runtime.Gosched();out=recover();n=7}();runtime.Gosched();panic("literal")}
//go:noinline
func namedOwner()(out any,n int){v:=3;defer asyncRecover(&out,&n,argument(v));v=99;runtime.Gosched();panic("named")}
//go:noinline
func maker(p *int,out *any,delta int)func(int){return func(n int){runtime.Gosched();*p+=n+delta;*out=recover()}}
//go:noinline
func captured()(out any,n int){f:=maker(&n,&out,4);defer f(argument(3));f=nil;runtime.Gosched();panic("captured")}
//go:noinline
func dynamicOwner(other bool)(out any,n int){f:=asyncRecover;if other{f=otherRecover};defer f(&out,&n,argument(3));f=asyncRecover;runtime.Gosched();panic("dynamic")}
//go:noinline
func mixedOwner(plain bool)(out any,n int){f:=asyncRecover;if plain{f=plainRecover};defer f(&out,&n,2);runtime.Gosched();panic("mixed")}
//go:noinline
func nilOwner(useNil bool)(out any){defer func(){if v:=recover();v!=nil{out=v}}();f:=asyncRecover;if useNil{f=nil};var n int;defer f(&out,&n,argument(2));runtime.Gosched();panic("outer")}
//go:noinline
func repeated()(out any,trace []int){defer func(){out=recover()}();for i:=0;i<4;i++{defer record(&trace,i)};runtime.Gosched();panic("repeat")}
//go:noinline
func values(y func(int)bool){for i:=1;i<=3;i++{if !y(i){return}}}
//go:noinline
func rangeOwner()(out any,trace []int){defer func(){out=recover()}();for v:=range values{defer record(&trace,v)};runtime.Gosched();panic("range")}
//go:noinline
func literalRepeated()(trace []int){for i:=0;i<4;i++{v:=i;defer func(){runtime.Gosched();trace=append(trace,v)}();v+=10};runtime.Gosched();return}
//go:noinline
func literalArguments()(out any,trace []int){defer func(){out=recover()}();for i:=0;i<3;i++{defer func(v int){runtime.Gosched();trace=append(trace,v);if v!=0{panic(v)}}(i)};runtime.Gosched();panic("body")}
//go:noinline
func immediateOwner()(out any){func(){defer publicRecover(&out);panic("immediate")}();return}
var saved func(int)bool
var callbackReady,callbackRelease chan struct{}
//go:noinline
func stash(y func(int)bool){saved=y;close(callbackReady);<-callbackRelease}
//go:noinline
func escapedOwner()(out any,trace []int){defer func(){out=recover()}();for v:=range stash{runtime.LockOSThread();defer record(&trace,v);runtime.UnlockOSThread()};panic("escape")}
func mixedThreads(locked bool){callbackReady=make(chan struct{});callbackRelease=make(chan struct{});done:=make(chan []int,1);go func(){if locked{runtime.LockOSThread();defer runtime.UnlockOSThread()};out,trace:=escapedOwner();if out!="escape"{panic("escaped owner")};done<-trace}();<-callbackReady;if !locked{runtime.LockOSThread()};saved(1);saved(2);if !locked{runtime.UnlockOSThread()};runtime.GC();close(callbackRelease);trace:=<-done;if len(trace)!=2||trace[0]!=2||trace[1]!=1{panic("escaped cleanup")}}
//go:noinline
func normalOwner()(out any){defer func(){runtime.Gosched();var nested any;publicRecover(&nested);if nested!=nil{panic("indirect stole owner panic")};out=recover()}();runtime.Gosched();panic("normal")}
//go:noinline
func recurseRecover(depth int,out *any){if depth>0{var nested any;recurseRecover(depth-1,&nested);if nested!=nil{panic("recursive normal recover")}};runtime.Gosched();*out=recover()}
//go:noinline
func recursiveOwner()(out any){defer recurseRecover(3,&out);runtime.Gosched();panic("recursive")}
//go:noinline
func nestedOwner()(out any){defer func(){out=recover()}();defer func(){defer func(){runtime.Gosched();if recover()!="inner"{panic("inner owner")}}();runtime.Gosched();panic("inner")}();runtime.Gosched();panic("outer")}
//go:noinline
func exitOwner(done chan int){defer func(){done<-1}();defer func(){runtime.Gosched();if recover()!=nil{panic("Goexit recovered")}}();runtime.Goexit()}
//go:noinline
func exitPanicOwner(done chan int){var out any;var n int;defer func(){if out!="cleanup"||n!=1{panic("exit replacement recover")};done<-2}();defer asyncRecover(&out,&n,1);defer asyncPanic();runtime.Goexit()}
//go:noinline
func nilPanicOwner()(out any){defer func(){runtime.Gosched();out=recover()}();runtime.Gosched();panic(nil)}
var publicFlag int
//go:noinline
func publicOrdinary()(out any){defer publicRecover(&out);if publicFlag!=0{goto fail};out="pending";fail:panic("public")}
//go:noinline
func parked(v int,ready,gate chan struct{},done chan int)(out any,n int){defer func(){if out!="park"{panic("park recovery")};done<-n}();defer asyncRecover(&out,&n,v);ready<-struct{}{};<-gate;panic("park")}
//go:noinline
func startParked(v int,ready,gate chan struct{},done chan int){go parked(v,ready,gate,done)}
func controls(){if immediateOwner()!="immediate"{panic("immediate closure recovery")};if t:=literalRepeated();len(t)!=4||t[0]!=13||t[1]!=12||t[2]!=11||t[3]!=10{panic("live repeated closure cells")};if out,t:=literalArguments();out!=1||len(t)!=3||t[0]!=2||t[1]!=1||t[2]!=0{panic("argument literal panic replacement")};evaluated=0;if onlyDefer()!=7{panic("only deferred suspension")};if out,n:=literal();out!="literal"||n!=7{panic("literal recover")};if out,n:=namedOwner();out!="named"||n!=3{panic("named operands")};if out,n:=captured();out!="captured"||n!=7{panic("captured selected callee")};for _,b:=range []bool{false,true}{out,n:=dynamicOwner(b);want:=3;if b{want=6};if out!="dynamic"||n!=want{panic("dynamic closed targets")};out,n=mixedOwner(b);want=2;if b{want=6};if out!="mixed"||n!=want{panic("mixed synchronous targets")}};if _,ok:=nilOwner(true).(runtime.Error);!ok{panic("nil invocation")};if nilOwner(false)!="outer"{panic("non-nil invocation")};if evaluated!=6{panic("frozen argument count")};if out,t:=repeated();out!="repeat"||len(t)!=4||t[0]!=3||t[1]!=2||t[2]!=1||t[3]!=0{panic("repeated LIFO")};if out,t:=rangeOwner();out!="range"||len(t)!=3||t[0]!=3||t[1]!=2||t[2]!=1{panic("range LIFO")};if normalOwner()!="normal"{panic("normal call isolation")};if recursiveOwner()!="recursive"{panic("recursive recover isolation")};if nestedOwner()!="outer"{panic("nested owner panic")};d:=make(chan int);go exitOwner(d);if <-d!=1{panic("exit cleanup")};go exitPanicOwner(d);if <-d!=2{panic("sticky exit cleanup")};if _,ok:=nilPanicOwner().(*runtime.PanicNilError);!ok{panic("nil panic recovery")};if publicOrdinary()!="public"{panic("ordinary public direct recover")}}
func main(){controls();runtime.LockOSThread();controls();runtime.UnlockOSThread();r:=make(chan struct{},32);g:=make(chan struct{});d:=make(chan int,32);for i:=0;i<32;i++{startParked(i,r,g,d)};for i:=0;i<32;i++{<-r};runtime.GC();close(g);sum:=0;for i:=0;i<32;i++{sum+=<-d};if sum!=496{panic("parked cleanup GC roots")};mixedThreads(false);mixedThreads(true);println("resumable-defer-calls-ok")}
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
				for _, name := range []string{"publicRecover", "asyncRecover", "otherRecover", "record", "asyncPanic", "onlyDefer", "literal", "namedOwner", "immediateOwner", "immediateOwner.func1", "literalRepeated", "literalArguments", "captured", "dynamicOwner", "mixedOwner", "nilOwner", "repeated", "rangeOwner", "escapedOwner", "normalOwner", "recurseRecover", "recursiveOwner", "nestedOwner", "exitOwner", "exitPanicOwner", "nilPanicOwner", "parked", "startParked"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
							t.Fatal("resumable defer lost required native admission")
						}
					})
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "resumable-defer-calls-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
