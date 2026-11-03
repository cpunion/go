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

func TestCapturedOperandCallees(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import("runtime";"context";"os";"os/exec";"strings";"time")
type box struct{n int;pad [2048]byte}
var evaluations int
//go:noinline
func argument()int{evaluations++;return 1}
//go:noinline
func maker(p *box,seed int)func(int)int{return func(v int)int{p.n+=seed+v;return p.n}}
//go:noinline
func regular()(n int){b:=&box{n:1};f:=maker(b,2);defer func(){n=b.n}();defer f(argument());f=maker(&box{n:1000},1000);runtime.Gosched();b.n+=10;return}
//go:noinline
func noargs()(n int){b:=&box{n:1};f:=func()int{b.n++;return b.n};defer func(){n=b.n}();defer f();f=nil;runtime.Gosched();return}
//go:noinline
func literal()(n int){for i:=1;i<=3;i++{defer func(v int){n+=v}(i)};runtime.Gosched();return}
//go:noinline
func returnedValues()(n int){b:=&box{n:1};defer func(){n=b.n}();f:=func(v int)(int,*box){b.n+=v;return b.n,b};defer f(3);runtime.Gosched();b.n+=5;return}
//go:noinline
func nilCall(useNil bool)(caught any){defer func(){caught=recover()}();b:=&box{n:1};f:=maker(b,2);if useNil{f=nil};defer f(argument());runtime.Gosched();return}
//go:noinline
func recoverLiteral()(caught any){p:=&caught;defer func(v int){*p=recover()}(7);runtime.Gosched();panic("direct")}
//go:noinline
func replacement()(caught any){defer func(){caught=recover()}();b:=&box{n:1};defer func(v string)int{if b.n==1{panic(v)};return b.n}("replacement");runtime.Gosched();panic("original")}
var exited int
//go:noinline
func exiting(done chan struct{}){defer func(){close(done)}();b:=&box{n:1};defer func(v int){exited=b.n+v;runtime.Goexit()}(7);runtime.Gosched();panic("suppressed")}
//go:noinline
func makeChild(p *box,seed int,ready chan int,release chan struct{},done chan int)func(int)int{return func(v int)int{ready<-seed;<-release;if p.pad[2047]!=byte(seed){panic("lost capture root")};p.n+=seed+v;done<-p.n;return p.n}}
//go:noinline
func spawn(p *box,seed int,ready chan int,release chan struct{},done chan int){f:=makeChild(p,seed,ready,release,done);go f(argument());f=nil}
//go:noinline
func makeZero(p *box,done chan int)func(){return func(){p.n++;done<-p.n}}
//go:noinline
func spawnZero(p *box,done chan int){f:=makeZero(p,done);go f();f=nil}
//go:noinline
func one(y func(int)bool){y(1)}
//go:noinline
func rangeOwner()(n int){for v:=range one{f:=func()int{n+=v;return n};defer f();f=nil};runtime.Gosched();return}
var saved func(int)bool
var ready,release chan struct{}
var trace int
//go:noinline
func stash(y func(int)bool){saved=y;close(ready);<-release}
//go:noinline
func escapedOwner(){for v:=range stash{runtime.LockOSThread();f:=func(){trace=10*trace+v};defer f();f=nil;runtime.UnlockOSThread()}}
func mixed(locked bool){trace=0;ready=make(chan struct{});release=make(chan struct{});done:=make(chan struct{});go func(){if locked{runtime.LockOSThread();defer runtime.UnlockOSThread()};escapedOwner();close(done)}();<-ready;if !locked{runtime.LockOSThread()};saved(1);saved(2);if !locked{runtime.UnlockOSThread()};runtime.GC();close(release);<-done;if trace!=21{panic("ordinary capture layout")}}
//go:noinline
func fatalMaker(p *box)func(int){return func(v int){p.n+=v;panic("captured-child-panic")}}
//go:noinline
func spawnFatal(useNil bool){f:=fatalMaker(&box{});if useNil{f=nil};go f(argument());runtime.Gosched()}
func inlineMaker(p *box,delta int)func(int)int{return func(v int)int{p.n+=delta+v;return p.n}}
//go:noinline
func inlinedOperand()(n int){b:=&box{n:1};f:=inlineMaker(b,2);defer func(){n=b.n}();defer f(3);runtime.Gosched();b.n+=4;return}
//go:noinline
func mutatingOperand()(n int){seed:=2;f:=func(v int)int{seed+=v;return seed};defer func(){n=seed}();defer f(3);seed=7;runtime.Gosched();return}
//go:noinline
func variadicOperand()(n int){b:=&box{n:1};f:=func(xs ...int)(int,*box){for _,v:=range xs{b.n+=v};return b.n,b};defer func(){n=b.n}();xs:=[]int{2,3};defer f(xs...);xs[1]=7;runtime.Gosched();return}
//go:noinline
func selected()func(int,int){trace=10*trace+1;p:=&trace;return func(a,b int){*p=10**p+a+b}}
//go:noinline
func operand(v int)int{trace=10*trace+v;return v}
//go:noinline
func orderedOperand(){defer selected()(operand(2),operand(3));runtime.Gosched();trace=10*trace+4}
func controls(){if inlinedOperand()!=10||mutatingOperand()!=10||variadicOperand()!=10{panic("captured live cells, variadic slice or inlined producer")};trace=0;orderedOperand();if trace!=12345{panic("callee and argument ordering")}}

func main(){
 if len(os.Args)>1{defer func(){if recover()!=nil{println("parent-recovered-child")}}();spawnFatal(os.Args[1]=="nil");select{}}
 controls();if regular()!=14||noargs()!=2||literal()!=6||returnedValues()!=9{panic("captured operand result")}
 before:=evaluations;if _,ok:=nilCall(true).(runtime.Error);!ok||evaluations!=before+1{panic("nil defer timing")};if nilCall(false)!=nil{panic("non-nil defer")}
 if recoverLiteral()!="direct"||replacement()!="replacement"{panic("deferred terminal ownership")}
 d:=make(chan struct{});go exiting(d);<-d;if exited!=8{panic("deferred Goexit")}
 r,g,done:=make(chan int,64),make(chan struct{}),make(chan int,64);for i:=0;i<64;i++{p:=&box{n:10};p.pad[2047]=byte(i);spawn(p,i,r,g,done)};for i:=0;i<64;i++{<-r};runtime.GC();close(g);sum:=0;for i:=0;i<64;i++{sum+=<-done};if sum!=2720{panic("spawn captures")}
 zero:=make(chan int,1);spawnZero(&box{n:4},zero);if <-zero!=5{panic("zero-argument spawn")}
 if rangeOwner()!=1{panic("enclosing range cleanup")};mixed(false);mixed(true)
 runtime.LockOSThread();controls();if regular()!=14||noargs()!=2||literal()!=6||rangeOwner()!=1{panic("ordinary source control")};runtime.UnlockOSThread()
 self,err:=os.Executable();if err!=nil{panic(err)};for _,mode:=range []string{"nil","panic"}{ctx,cancel:=context.WithTimeout(context.Background(),30*time.Second);data,err:=exec.CommandContext(ctx,self,mode).CombinedOutput();expired:=ctx.Err()!=nil;cancel();text:=string(data);want:="invalid memory address or nil pointer dereference";if mode=="panic"{want="captured-child-panic"};if expired||err==nil||!strings.Contains(text,want)||strings.Contains(text,"parent-recovered-child"){panic("child panic ownership: "+text)}}
 println("captured-operands-ok")
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
				for _, name := range []string{"regular", "noargs", "literal", "returnedValues", "nilCall", "recoverLiteral", "replacement", "exiting", "spawn", "spawnZero", "rangeOwner", "escapedOwner", "spawnFatal", "inlinedOperand", "mutatingOperand", "variadicOperand", "orderedOperand"} {
					t.Run(name, func(t *testing.T) {
						if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
							t.Fatal("captured go/defer operand lost native admission")
						}
					})
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "captured-operands-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
