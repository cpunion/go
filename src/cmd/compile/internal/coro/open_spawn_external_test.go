// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"context"
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise source go statements with unknown target sets. Every required
// source owner must retain an admitted linked native entry; successful ordinary
// execution alone cannot satisfy this contract.
func TestOpenFunctionSpawns(t *testing.T) {
	const source = `package main

import (
 "os"
 "runtime"
 "sync/atomic"
)

type callback func(chan int,int)
type box struct { n int; pad [4096]byte }
var visits int
var finalized atomic.Int32

func nativeSend(ch chan int,n int) { runtime.Gosched();ch<-n }
func opaqueSend(ch chan int,n int) { var stack [2048]byte; if runtime.Stack(stack[:],false)==0 { panic("opaque stack") }; runtime.Gosched();ch<-n+1 }
func capture(p *box) callback { return func(ch chan int,n int) { runtime.Gosched();runtime.GC();ch<-p.n+n+int(p.pad[4095]) } }
func(p *box) send(ch chan int,n int) { runtime.Gosched();ch<-p.n+n }
func tuple(ch chan int)(int,*box) { p:=&box{n:41};runtime.Gosched();ch<-p.n;return 43,p }
func variadic(ch chan int,values ...int) { sum:=0;for _,n:=range values {runtime.Gosched();sum+=n};ch<-sum }
func parked(release <-chan struct{},ready chan<-struct{},done chan<-int,p *box) {
 var stack [2048]byte; if runtime.Stack(stack[:],false)==0 {panic("parked stack")}
 runtime.LockOSThread();defer runtime.UnlockOSThread()
 ready<-struct{}{};<-release
 for i:=0;i<8;i++ {runtime.Gosched()}
 done<-p.n+int(p.pad[4095])
}
func exiting(done chan int) { defer func(){done<-47}();runtime.Goexit() }
func opaqueExit(done chan int) {var stack[1024]byte;if runtime.Stack(stack[:],false)==0{panic("exit stack")};defer func(){done<-53}();runtime.Goexit()}
func panicking(done chan int) {defer func(){done<-61}();runtime.Gosched();panic("child-native-panic")}
func opaquePanic(done chan int) {var stack[1024]byte;if runtime.Stack(stack[:],false)==0{panic("panic stack")};defer func(){done<-67}();runtime.Gosched();panic("child-opaque-panic")}

func spawnOwner(f callback,ch chan int,n int) { visits++;go f(ch,n);runtime.Gosched();visits++ }
func methodOwner(f func(*box,chan int,int),p *box,ch chan int,n int) {go f(p,ch,n);runtime.Gosched()}
func tupleOwner(f func(chan int)(int,*box),ch chan int) {go f(ch);runtime.Gosched()}
func variadicOwner(f func(chan int,...int),ch chan int) {go f(ch,3,5,7);runtime.Gosched()}
func pressureOwner(f func(<-chan struct{},chan<-struct{},chan<-int,*box),release <-chan struct{},ready chan<-struct{},done chan<-int,p *box) {go f(release,ready,done,p);runtime.Gosched()}
func exitOwner(f func(chan int),done chan int) {go f(done);runtime.Gosched()}
func panicOwner(f func(chan int),done chan int) {defer func(){if recover()!=nil{println("parent-recovered")}}();go f(done);runtime.Gosched();<-done;select{}}
var chosen callback
var producerCalls,argumentCalls int
func replace()int { argumentCalls++;chosen=opaqueSend;return 7 }
func snapshotOwner(produce func()callback,arg func()int,ch chan int) {go produce()(ch,arg());runtime.Gosched()}
func lateVariableOwner(ch chan int) {go chosen(ch,replace());runtime.Gosched()}
func produce()callback {producerCalls++;return chosen}
func nilOwner(f func(int),arg func()int) {defer func(){if recover()!=nil{println("parent-recovered")}}();go f(arg());select{}}
func nilEmptyOwner(f func()) {defer func(){if recover()!=nil{println("parent-recovered")}}();go f();select{}}
func argument()int {println("argument-evaluated");return 9}

var spawnEntry=spawnOwner
var methodEntry=methodOwner
var tupleEntry=tupleOwner
var variadicEntry=variadicOwner
var pressureEntry=pressureOwner
var exitEntry=exitOwner
var panicEntry=panicOwner
var snapshotEntry=snapshotOwner
var lateVariableEntry=lateVariableOwner
var nilEntry=nilOwner
var nilEmptyEntry=nilEmptyOwner

func oneParked(release <-chan struct{},ready chan<-struct{},done chan<-int,n int) {
 p:=&box{n:n};p.pad[4095]=17
 runtime.SetFinalizer(p,func(*box){finalized.Add(1)})
 pressureEntry(parked,release,ready,done,p)
}
func main() {
 switch os.Getenv("OPEN_SPAWN_FAILURE") {
 case "nil-argument":nilEntry(nil,argument);panic("nil go returned")
 case "nil-empty":nilEmptyEntry(nil);panic("nil go returned")
 case "panic-native":panicEntry(panicking,make(chan int));panic("child panic returned")
 case "panic-opaque":panicEntry(opaquePanic,make(chan int));panic("child panic returned")
 }
 ch:=make(chan int)
 spawnEntry(nativeSend,ch,19);if <-ch!=19||visits!=2{panic("native go or parent replay")}
 spawnEntry(opaqueSend,ch,23);if <-ch!=24||visits!=4{panic("opaque go or parent replay")}
 p:=&box{n:29};p.pad[4095]=11;f:=capture(p);p=nil
 spawnEntry(f,ch,7);f=nil;if <-ch!=47||visits!=6{panic("captured go environment")}
 methodEntry((*box).send,&box{n:31},ch,5);if <-ch!=36{panic("method expression go")}
 tupleEntry(tuple,ch);if <-ch!=41{panic("discarded go tuple")}
 variadicEntry(variadic,ch);if <-ch!=15{panic("variadic go")}
 chosen=nativeSend;producerCalls=0;argumentCalls=0;snapshotEntry(produce,replace,ch);if <-ch!=7||producerCalls!=1||argumentCalls!=1{panic("go callee snapshot or argument replay")}
 chosen=nativeSend;producerCalls=0;argumentCalls=0;lateVariableEntry(ch);if <-ch!=8||producerCalls!=0||argumentCalls!=1{panic("go late function variable read or replay")}
 exitEntry(exiting,ch);if <-ch!=47{panic("native goexit cleanup")};exitEntry(opaqueExit,ch);if <-ch!=53{panic("opaque goexit cleanup")}
 const count=32
 release:=make(chan struct{});ready:=make(chan struct{});done:=make(chan int)
 for i:=0;i<count;i++ {oneParked(release,ready,done,i)}
 for i:=0;i<count;i++ {<-ready}
 runtime.GC();runtime.GC();for i:=0;i<4;i++ {runtime.Gosched()}
 if finalized.Load()!=0{panic("detached opaque child lost traced payload")}
 close(release);sum:=0;for i:=0;i<count;i++ {sum+=<-done}
 if sum!=count*(count-1)/2+count*17{panic("opaque child payload corruption")}
 println("open-spawn-ok")
}
`
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	file := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(file, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags, extra string }{
		{"native", "coro", "-l -d=coro=4", ""},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2", ""},
		{"inline", "coro", "-d=coro=4", ""},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2", ""},
		{"no-dwarf", "coro", "-d=coro=4", "-ldflags=-w"},
		{"pie", "coro", "-l -d=coro=4", "-buildmode=pie"},
		{"ordinary", "nocoro", "-l", ""},
		{"ordinary-inline", "nocoro", "", ""},
	} {
		t.Run(config.name, func(t *testing.T) {
			executable := filepath.Join(tmp, config.name)
			args := []string{"build", "-gcflags=" + config.flags, "-o", executable}
			if config.extra != "" {
				args = append(args, config.extra)
			}
			build := testenv.Command(t, testenv.GoToolPath(t), append(args, file)...)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"spawnOwner", "methodOwner", "tupleOwner", "variadicOwner", "pressureOwner", "exitOwner", "snapshotOwner", "lateVariableOwner", "nilOwner", "nilEmptyOwner", "panicOwner", "panicking", "nativeSend", "tuple", "variadic", "exiting"} {
					if linkedCoroStackUnsafe(t, executable, "main."+name+".coro.stackgate") {
						t.Errorf("open go source lost native admission: %s", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				for _, mode := range []string{"", "nil-argument", "nil-empty", "panic-native", "panic-opaque"} {
					ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
					run := testenv.CommandContext(t, ctx, executable)
					run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1", "OPEN_SPAWN_FAILURE="+mode)
					data, err := run.CombinedOutput()
					timedOut := ctx.Err() != nil
					cancel()
					if mode == "" {
						if err != nil || strings.TrimSpace(string(data)) != "open-spawn-ok" {
							t.Fatalf("P%s: %v\n%s", p, err, data)
						}
					} else if timedOut || err == nil || strings.Contains(string(data), "parent-recovered") ||
						mode == "nil-argument" && strings.Count(string(data), "argument-evaluated") != 1 ||
						mode == "panic-native" && !strings.Contains(string(data), "child-native-panic") ||
						mode == "panic-opaque" && !strings.Contains(string(data), "child-opaque-panic") {
						t.Fatalf("P%s %s changed child terminal outcome: %v\n%s", p, mode, err, data)
					}
				}
			}
		})
	}
}
