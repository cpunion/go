// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This tests discovery through the real compiler/linker and runtime funcdata.
// It deliberately does not stand in for open call-site or interface admission.
func TestCallableCompanionMetadata(t *testing.T) {
	const source = `package main
import("reflect";"runtime";"unsafe")

// The fixture inspects a private compiler/runtime ABI. Disabling linkname
// checking below does not make this an application-facing runtime interface.
//go:linkname lookup runtime.coroLookupCallable
func lookup(uintptr,unsafe.Pointer)unsafe.Pointer

func named(n int)int{runtime.Gosched();return n+3}
func plain(n int)int{return n+5}
func large(value [1024]byte)int{runtime.Gosched();return int(value[0])}
type box struct{n int;pad[2047]byte}
func(p *box)add(n int)int{runtime.Gosched();p.n+=n;return p.n}
func captured(p *box)func(int)int{return func(n int)int{runtime.Gosched();return p.n+n}}

func signature(f any)unsafe.Pointer{return (*[2]unsafe.Pointer)(unsafe.Pointer(&f))[0]}
func descriptor(f any)unsafe.Pointer{return lookup(reflect.ValueOf(f).Pointer(),signature(f))}
func check(f any){
 p:=descriptor(f);if p==nil{panic("missing callable descriptor")}
 words:=(*[6]uintptr)(p)
 if words[0]!=2||words[1]!=uintptr(signature(f))||words[2]==0||words[3]==0||words[4]==0||words[5]==0{panic("callable descriptor identity")}
 gate:=(*[2]uintptr)(unsafe.Pointer(words[2]))
 if gate[0]!=0||gate[1]!=words[3]{panic("erased adapter has no independent accepted stack proof")}
 deferred:=(*[2]uintptr)(unsafe.Pointer(words[4]))
 if deferred[0]!=0||deferred[1]!=words[5]||words[2]==words[4]||words[3]==words[5]{panic("direct defer has no independent accepted capability")}
 code:=reflect.ValueOf(f).Pointer()
 if lookup(code+1,signature(f))!=nil{panic("interior code pointer accepted")}
 if lookup(code,signature((func(string)int)(nil)))!=nil{panic("wrong callable signature accepted")}
 if lookup(code,nil)!=nil||lookup(0,signature(f))!=nil{panic("missing callable identity accepted")}
}

func main(){
 for range 1{
  p:=&box{n:11};p.pad[2046]=17;f:=captured(p)
  check(named);check(f);check((*box).add)
  if descriptor(plain)!=nil{panic("plain function acquired an unproved factory")}
  if descriptor(large)!=nil{panic("large transfer acquired native capability")}
  runtime.GC()
  if named(7)!=10||f(13)!=24||(*box).add(p,2)!=13||plain(4)!=9||large([1024]byte{19})!=19||p.pad[2046]!=17{panic("ordinary source behavior changed")}
  runtime.LockOSThread();if f(2)!=15||named(3)!=6{panic("ordinary thread entry changed")};runtime.UnlockOSThread()
 }
 println("callable-metadata-ok")
}
`
	testCallableProgramme(t, source, "callable-metadata-ok")
}

// Invoke the erased factory through the actual runtime bridge, then await its
// generated continuation. This is an ABI test with a hand-written root driver;
// it does not claim that source open calls or interface calls are lowered.
func TestCallableCompanionInvocation(t *testing.T) {
	const source = `package main
import("reflect";"runtime";"unsafe")

//go:linkname lookup runtime.coroLookupCallable
func lookup(uintptr,unsafe.Pointer)unsafe.Pointer
//go:linkname factory runtime.coroCallableFactory
func factory(unsafe.Pointer,unsafe.Pointer,unsafe.Pointer,unsafe.Pointer)(unsafe.Pointer,func(unsafe.Pointer)uint8)
//go:linkname await runtime.coroAwaitFrame
func await(unsafe.Pointer,unsafe.Pointer,func(unsafe.Pointer)uint8)
//go:linkname frame runtime.coroFrame
func frame(unsafe.Pointer)unsafe.Pointer
//go:linkname run runtime.coroRunFrame
func run(unsafe.Pointer,func(unsafe.Pointer)uint8,uintptr)bool

// This private runtime entry starts a root from an ordinary Go stack. Keep
// the hand-written driver outside source callback specialization and native
// root lowering; the child factory still executes in the actual native root.
var ordinaryRootDriver = run

type box struct{n int;pad[4095]byte}
func named(n int)int{runtime.Gosched();runtime.GC();return n+3}
func captured(p *box)func(int)int{return func(n int)int{runtime.Gosched();runtime.GC();return p.n+n+int(p.pad[4094])}}
func(p *box)add(n int)int{runtime.Gosched();runtime.GC();p.n+=n;return p.n}
func pair(p *box,n int)(int,*box){runtime.Gosched();runtime.GC();return p.n+n,p}
func sum(n int,values ...int)int{for _,v:=range values{runtime.Gosched();n+=v};return n}
func store(p *box,n int){runtime.Gosched();runtime.GC();p.n=n}
//go:noinline
func read(p *[5120]byte)int{return int(p[0])+int(p[5119])}
func sequence(n,seed int)int{
 var data [5120]byte
 data[5119]=byte(seed)
 seed+=7
 if n==0{runtime.Gosched();return read(&data)+read(&data)}
 got:=sequence(n-1,seed)
 return got+read(&data)+read(&data)
}

type invocation struct{
 pc uint8
 descriptor,environment,request unsafe.Pointer
 done bool
}

func drive(ctx unsafe.Pointer)uint8{
 state:=(*invocation)(frame(ctx))
 switch state.pc{
 case 0:
  child,resume:=factory(ctx,state.descriptor,state.environment,state.request)
  if resume==nil{panic("missing native callable continuation")}
  state.pc=1
  await(ctx,child,resume)
  return 2
 case 1:
  state.done=true
  return 3
 }
 panic("invalid callable test continuation")
}

func invoke(f any,request unsafe.Pointer){
 words:=(*[2]unsafe.Pointer)(unsafe.Pointer(&f))
 descriptor:=lookup(reflect.ValueOf(f).Pointer(),words[0])
 if descriptor==nil{panic("missing admitted callable")}
 state:=&invocation{descriptor:descriptor,environment:words[1],request:request}
 // The environment, typed request, child frame, and typed results must keep
 // every pointer live after the creator has discarded the function value.
 f=nil
 runtime.GC()
 ordinaryRootDriver(unsafe.Pointer(state),drive,0)
 if !state.done{panic("callable child did not return to its parent")}
}

func main(){
 result:=0
 one:=struct{n int;result *int}{7,&result}
 invoke(named,unsafe.Pointer(&one));if result!=10{panic("named result")}
 p:=&box{n:11};p.pad[4094]=17
 f:=captured(p);p=nil
 invoke(f,unsafe.Pointer(&one));f=nil
 if result!=35{panic("captured environment or result")}
 p=&box{n:19};p.pad[4094]=23
 method:=struct{p *box;n int;result *int}{p,5,&result}
 invoke((*box).add,unsafe.Pointer(&method));if result!=24||p.n!=24||p.pad[4094]!=23{panic("method receiver")}
 var returned *box
 tuple:=struct{p *box;n int;result *int;returned **box}{p,7,&result,&returned}
 invoke(pair,unsafe.Pointer(&tuple));if result!=31||returned!=p||returned.pad[4094]!=23{panic("typed tuple result")}
 variadic:=struct{n int;values []int;result *int}{2,[]int{3,5,7},&result}
 invoke(sum,unsafe.Pointer(&variadic));if result!=17{panic("variadic request")}
 empty:=struct{p *box;n int}{p,37}
 invoke(store,unsafe.Pointer(&empty));if p.n!=37{panic("empty result request")}
 runtime.GC();if returned.n!=37||returned.pad[4094]!=23{panic("result pointer liveness")}
 for _,n:=range []int{0,1,16,64,513}{
  for seed:=0;seed<4;seed++{
   request:=struct{n,seed int;result *int}{n,seed,&result}
   invoke(sequence,unsafe.Pointer(&request))
   want:=0;for i:=0;i<=n;i++{want+=2*int(byte(seed+7*i))}
   if result!=want{panic("erased callable array root or recursive snapshot")}
  }
 }
 println("callable-invocation-ok")
}
`
	testCallableProgramme(t, source, "callable-invocation-ok")
}

func testCallableProgramme(t *testing.T, source, want string) {
	t.Helper()
	testenv.MustHaveGoBuild(t)
	tmp := t.TempDir()
	path := filepath.Join(tmp, "main.go")
	if err := os.WriteFile(path, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, flags, link string }{
		{"native", "-l", ""},
		{"checkptr", "-l -d=checkptr=2", ""},
		{"inline", "", ""},
		{"checkptr-inline", "-d=checkptr=2", ""},
		{"no-dwarf", "", "-w"},
		{"pie", "-l", "-buildmode=pie"},
	} {
		t.Run(config.name, func(t *testing.T) {
			exe := filepath.Join(tmp, config.name+".exe")
			args := []string{"build", "-gcflags=" + config.flags + " -d=coro=4", "-ldflags=-checklinkname=0"}
			if config.link == "-w" {
				args[len(args)-1] += " -w"
			} else if config.link != "" {
				args = append(args, config.link)
			}
			args = append(args, "-o", exe, path)
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Env = append(build.Environ(), "GOEXPERIMENT=coro", "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, exe)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				if data, err := run.CombinedOutput(); err != nil || strings.TrimSpace(string(data)) != want {
					t.Fatalf("P%s: %v\n%s", p, err, data)
				}
			}
		})
	}
}
