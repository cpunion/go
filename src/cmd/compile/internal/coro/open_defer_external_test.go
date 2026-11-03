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

// No private runtime ABI is used by this source. Check source owners and
// direct-defer callees independently; ordinary output alone is insufficient.
func TestOpenFunctionDefers(t *testing.T) {
	const source = `package main
import("runtime";"sync/atomic";"syscall")
type state struct{log []int;caught,outer any;token,replacement *int;body,returned bool;calls int;payload *[4096]byte;gid uint64;tid uintptr}
type cleanup func(*state)
var finalized atomic.Int32
func nativeRecover(s *state){runtime.Gosched();runtime.GC();s.caught=recover();s.log=append(s.log,1)}
func nativeReplace(s *state){runtime.Gosched();s.log=append(s.log,1);panic(s.replacement)}
func noSuspendReplace(s *state){s.caught=recover();s.log=append(s.log,1);panic(s.replacement)}
func nativeExit(s *state){runtime.Gosched();s.log=append(s.log,1);runtime.Goexit()}
//go:noinline
func indirect()any{return recover()}
func nativeIndirect(s *state){runtime.Gosched();if indirect()!=nil{panic("indirect borrowed owner panic")};s.log=append(s.log,1)}
func tail(s *state){runtime.Gosched();s.log=append(s.log,2)}
func opaqueRecover(s *state){var stack[2048]byte;if runtime.Stack(stack[:],false)==0{panic("opaque stack")};runtime.Gosched();runtime.GC();s.caught=recover();s.log=append(s.log,1)}
func opaqueReplace(s *state){var stack[2048]byte;if runtime.Stack(stack[:],false)==0{panic("opaque stack")};s.log=append(s.log,1);panic(s.replacement)}
func opaqueExit(s *state){var stack[2048]byte;if runtime.Stack(stack[:],false)==0{panic("opaque stack")};s.log=append(s.log,1);runtime.Goexit()}
func owner(f cleanup,s *state,mode int){defer tail(s);defer f(s);s.body=true;runtime.Gosched();if mode==1{panic(s.token)};if mode==2{runtime.Goexit()}}
func bareOwner(f cleanup,s *state){defer f(s);s.body=true}
func bareNilOwner(f func(),s *state){defer func(){s.log=append(s.log,2)}();defer f();s.body=true}
func stickyOwner(replace,catch cleanup,s *state){defer tail(s);defer catch(s);defer replace(s);runtime.Gosched();runtime.Goexit()}
func nilOwner(f func(int),s *state){defer func(){s.log=append(s.log,2)}();defer f(argument(s));s.body=true;runtime.Gosched()}
func argument(s *state)int{s.calls++;return 7}
func record(s *state,n int){runtime.Gosched();runtime.GC();s.log=append(s.log,n)}
func repeatedOwner(f func(*state,int),s *state){for i:=0;i<6;i++{defer f(s,i);runtime.Gosched()}}
func iterator(yield func(int)bool){for i:=0;i<6;i++{runtime.Gosched();if !yield(i){return}}}
func rangeOwner(f func(*state,int),s *state){defer record(s,9);for i:=range iterator{defer f(s,i);runtime.Gosched()};defer record(s,8)}
func tuple(s *state)(int,*state){runtime.Gosched();s.log=append(s.log,3);return 17,s}
func tupleOwner(f func(*state)(int,*state),s *state){defer f(s);runtime.Gosched()}
func big(data [1024]byte,s *state){runtime.Gosched();runtime.GC();s.log=append(s.log,int(data[0])+int(data[1023]))}
func bigOwner(f func([1024]byte,*state),s *state){var data[1024]byte;data[0]=17;data[1023]=19;defer f(data,s);data[0]=99;runtime.Gosched()}
func variadic(s *state,values ...int){for _,n:=range values{runtime.Gosched();s.log=append(s.log,n)}}
func variadicOwner(f func(*state,...int),s *state){defer f(s,3,5,7);runtime.Gosched()}
type box struct{n int;pad[4096]byte}
func(p *box)touch(s *state,n int){runtime.Gosched();runtime.GC();s.log=append(s.log,p.n+n+int(p.pad[4095]))}
func methodOwner(f func(*box,*state,int),p *box,s *state){defer f(p,s,7);runtime.Gosched()}
func captured(p *box)cleanup{return func(s *state){runtime.Gosched();runtime.GC();s.caught=recover();s.log=append(s.log,p.n+int(p.pad[4095]))}}
var chosen func(*state,int)
var producerCalls,argumentCalls int
func produce()func(*state,int){producerCalls++;return chosen}
func replace()int{argumentCalls++;chosen=recordPlus;return 7}
func recordPlus(s *state,n int){runtime.Gosched();s.log=append(s.log,n+1)}
func snapshotOwner(producer func()func(*state,int),arg func()int,s *state){defer producer()(s,arg());runtime.Gosched()}
func lateOwner(s *state){defer chosen(s,replace());runtime.Gosched()}
func goid()uint64{var data[96]byte;n:=runtime.Stack(data[:],false);var id uint64;for i:=10;i<n&&data[i]>='0'&&data[i]<='9';i++{id=id*10+uint64(data[i]-'0')};if id==0{panic("missing G identity")};return id}
func thread()uintptr{number:=uintptr(186);if runtime.GOOS=="darwin"{number=372}else if runtime.GOARCH=="arm64"{number=178};id,_,err:=syscall.RawSyscall(number,0,0,0);if err!=0||id==0{panic("missing thread identity")};return id}
func lock(s *state){runtime.LockOSThread();s.gid=goid();s.tid=thread()}
func unlock(s *state){if s.gid!=goid()||s.tid!=thread(){panic("deferred call changed logical G/thread")};runtime.UnlockOSThread();s.log=append(s.log,4)}
func affinityOwner(start,finish cleanup,s *state){start(s);defer finish(s);for i:=0;i<8;i++{runtime.Gosched();runtime.GC()}}
func parked(s *state,ready chan<-struct{},release <-chan struct{}){runtime.LockOSThread();defer runtime.UnlockOSThread();gid,tid:=goid(),thread();ready<-struct{}{};<-release;for i:=0;i<8;i++{runtime.Gosched()};runtime.GC();if goid()!=gid||thread()!=tid||s.payload[4095]!=37{panic("parked defer lost environment or affinity")};s.log=append(s.log,int(s.payload[0]))}
func pressureOwner(f func(*state,chan<-struct{},<-chan struct{}),s *state,ready chan<-struct{},release <-chan struct{}){defer f(s,ready,release);runtime.Gosched()}
var ownerEntry=owner
var bareEntry=bareOwner
var bareNilEntry=bareNilOwner
var stickyEntry=stickyOwner
var nilEntry=nilOwner
var repeatedEntry=repeatedOwner
var rangeEntry=rangeOwner
var tupleEntry=tupleOwner
var bigEntry=bigOwner
var variadicEntry=variadicOwner
var methodEntry=methodOwner
var snapshotEntry=snapshotOwner
var lateEntry=lateOwner
var affinityEntry=affinityOwner
var pressureEntry=pressureOwner
func fresh()*state{return &state{token:new(int),replacement:new(int)}}
func execute(f func(*state),s *state){done:=make(chan struct{});go func(){defer close(done);defer func(){s.outer=recover()}();f(s);s.returned=true}();<-done}
func equal(got []int,want ...int){if len(got)!=len(want){panic("cleanup log length")};for i,n:=range want{if got[i]!=n{panic("cleanup LIFO order")}}}
func main(){
 for _,f:=range []cleanup{nativeRecover,opaqueRecover}{for mode:=0;mode<3;mode++{s:=fresh();execute(func(s *state){ownerEntry(f,s,mode)},s);equal(s.log,1,2);if !s.body||s.outer!=nil||s.returned!=(mode!=2){panic("recover terminal or owner replay")};if mode==1{if s.caught!=s.token{panic("direct recover lost panic")}}else if s.caught!=nil{panic("recover invented panic")}}}
 for _,f:=range []cleanup{nativeReplace,opaqueReplace}{for mode:=0;mode<3;mode++{s:=fresh();execute(func(s *state){ownerEntry(f,s,mode)},s);equal(s.log,1,2);if s.outer!=s.replacement||s.returned{panic("replacement panic")}}}
 for mode:=0;mode<3;mode++{s:=fresh();execute(func(s *state){ownerEntry(noSuspendReplace,s,mode)},s);equal(s.log,1,2);if s.outer!=s.replacement||s.returned{panic("NoSuspend replacement panic")};if mode==1{if s.caught!=s.token{panic("NoSuspend direct recover lost owner")}}else if s.caught!=nil{panic("NoSuspend recover invented panic")}}
 for _,f:=range []cleanup{nativeExit,opaqueExit}{for mode:=0;mode<3;mode++{s:=fresh();execute(func(s *state){ownerEntry(f,s,mode)},s);equal(s.log,1,2);if s.outer!=nil||s.returned{panic("deferred Goexit returned")}}}
 s:=fresh();execute(func(s *state){ownerEntry(nativeIndirect,s,1)},s);equal(s.log,1,2);if s.outer!=s.token||s.returned{panic("indirect recover consumed panic")}
 for _,f:=range []cleanup{nativeReplace,opaqueReplace}{for _,catch:=range []cleanup{nativeRecover,opaqueRecover}{s:=fresh();execute(func(s *state){stickyEntry(f,catch,s)},s);equal(s.log,1,1,2);if s.caught!=s.replacement||s.outer!=nil||s.returned{panic("sticky Goexit was recovered or panic lost")}}}
 s=fresh();execute(func(s *state){nilEntry(nil,s)},s);equal(s.log,2);if _,ok:=s.outer.(runtime.Error);!ok||!s.body||s.calls!=1||s.returned{panic("nil defer timing or arguments")}
 s=fresh();repeatedEntry(record,s);equal(s.log,5,4,3,2,1,0)
 s=fresh();rangeEntry(record,s);equal(s.log,8,5,4,3,2,1,0,9)
 s=fresh();tupleEntry(tuple,s);equal(s.log,3)
 s=fresh();bigEntry(big,s);equal(s.log,36)
 s=fresh();bareEntry(nativeRecover,s);equal(s.log,1);if !s.body||s.caught!=nil{panic("no-yield defer owner")}
 s=fresh();execute(func(s *state){bareNilEntry(nil,s)},s);equal(s.log,2);if _,ok:=s.outer.(runtime.Error);!ok||!s.body||s.returned{panic("bare nil defer timing")}
 s=fresh();variadicEntry(variadic,s);equal(s.log,3,5,7)
 s=fresh();p:=&box{n:19};p.pad[4095]=11;methodEntry((*box).touch,p,s);equal(s.log,37)
 s=fresh();f:=captured(p);p=nil;execute(func(s *state){ownerEntry(f,s,1)},s);f=nil;equal(s.log,30,2);if s.caught!=s.token||!s.returned{panic("captured direct recover")}
 s=fresh();p=&box{n:23};p.pad[4095]=13;bound:=p.touch;p=nil;repeatedEntry(bound,s);equal(s.log,41,40,39,38,37,36)
 s=fresh();chosen=record;producerCalls=0;argumentCalls=0;snapshotEntry(produce,replace,s);equal(s.log,7);if producerCalls!=1||argumentCalls!=1{panic("defer callee snapshot or argument replay")}
 s=fresh();chosen=record;argumentCalls=0;lateEntry(s);equal(s.log,8);if argumentCalls!=1{panic("late defer function variable or argument replay")}
 s=fresh();affinityEntry(lock,unlock,s);equal(s.log,4)
 const count=32;ready:=make(chan struct{});release:=make(chan struct{});done:=make(chan *state)
 for i:=0;i<count;i++{s:=fresh();s.payload=new([4096]byte);s.payload[0]=byte(i);s.payload[4095]=37;runtime.SetFinalizer(s.payload,func(*[4096]byte){finalized.Add(1)});go func(){pressureEntry(parked,s,ready,release);done<-s}()}
 for i:=0;i<count;i++{<-ready};runtime.GC();runtime.GC();if finalized.Load()!=0{panic("pending defer lost typed payload")};close(release);sum:=0;for i:=0;i<count;i++{s:=<-done;if len(s.log)!=1{panic("replayed pressure cleanup")};sum+=s.log[0]};if sum!=count*(count-1)/2{panic("pressure results")}
 println("open-defer-ok")
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
				for _, name := range []string{"owner", "bareOwner", "bareNilOwner", "bigOwner", "stickyOwner", "nilOwner", "repeatedOwner", "rangeOwner", "tupleOwner", "variadicOwner", "methodOwner", "snapshotOwner", "lateOwner", "affinityOwner", "pressureOwner", "nativeRecover", "nativeReplace", "noSuspendReplace", "nativeExit", "nativeIndirect", "record", "tuple", "variadic", "captured.func1"} {
					if linkedCoroStackUnsafe(t, executable, "main."+name+".coro.stackgate") {
						t.Errorf("open defer source lost native admission: %s", name)
					}
				}
				for _, name := range []string{"nativeRecover", "nativeReplace", "noSuspendReplace", "nativeExit", "nativeIndirect", "record", "tuple", "variadic", "captured.func1"} {
					if linkedCoroStackUnsafe(t, executable, "main."+name+".coro.defer.coro.stackgate") {
						t.Errorf("deferred callee has no independently admitted companion: %s", name)
					}
				}
				if !linkedCoroStackUnsafe(t, executable, "main.big.coro.defer.coro.stackgate") {
					t.Fatal("oversized deferred transfer acquired native capability")
				}
			}
			for _, p := range []string{"1", "4"} {
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				run := testenv.CommandContext(t, ctx, executable)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				data, err := run.CombinedOutput()
				cancel()
				if err != nil || strings.TrimSpace(string(data)) != "open-defer-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
