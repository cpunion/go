// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// These fixtures exercise the runtime handoff ABI. Source open call/interface
// lowering and its original native admission contracts remain separate gates.
func TestCallableOrdinaryHostAffinity(t *testing.T) {
	const source = `package main
import("runtime";"sync";"syscall";"unsafe")
//go:linkname host runtime.coroHostCall
func host(unsafe.Pointer,func())uint8
//go:linkname spawn runtime.coroSpawnFrame
func spawn(unsafe.Pointer,unsafe.Pointer,func(unsafe.Pointer)uint8)bool
//go:linkname await runtime.coroAwaitFrame
func await(unsafe.Pointer,unsafe.Pointer,func(unsafe.Pointer)uint8)
//go:linkname frame runtime.coroFrame
func frame(unsafe.Pointer)unsafe.Pointer
//go:linkname run runtime.coroRunFrame
func run(unsafe.Pointer,func(unsafe.Pointer)uint8,uintptr)bool
var ordinaryRootDriver=run

func goid()uint64{
 var data [96]byte
 n:=runtime.Stack(data[:],false)
 var id uint64
 for i:=10;i<n&&data[i]>='0'&&data[i]<='9';i++{id=id*10+uint64(data[i]-'0')}
 if id==0{panic("missing ordinary goroutine identity")};return id
}
func thread()uintptr{
 number:=uintptr(186)
 if runtime.GOOS=="darwin"{number=372}else if runtime.GOARCH=="arm64"{number=178}
 id,_,err:=syscall.RawSyscall(number,0,0,0)
 if err!=0||id==0{panic("missing ordinary thread identity")};return id
}

type pressure struct{condition *sync.Cond;ready,done int;release bool}
type child struct{pc,yields int;gid uint64;tid uintptr;p *pressure;payload *[4096]byte;finished bool}
func childResume(ctx unsafe.Pointer)uint8{
 state:=(*child)(frame(ctx))
 switch state.pc{
 case 0:
  state.pc=1
  return host(ctx,func(){runtime.LockOSThread();state.gid=goid();state.tid=thread();runtime.GC()})
 case 1:
  state.yields++
  if state.yields<8{return 1}
  state.pc=2
  return host(ctx,func(){
   if goid()!=state.gid||thread()!=state.tid{panic("logical goroutine migrated after opaque call")}
   runtime.UnlockOSThread()
   p:=state.p;p.condition.L.Lock();p.ready++;p.condition.Broadcast()
   for !p.release{p.condition.Wait()}
   if state.payload[4095]!=37{panic("opaque parked payload lost")}
   p.done++;p.condition.Broadcast();p.condition.L.Unlock()
  })
 case 2:
  if goid()!=state.gid{panic("native continuation changed host goroutine")}
  state.finished=true
  return 3
 }
 panic("invalid ordinary host child state")
}
type nested struct{pc int;gid uint64;done bool}
func nestedResume(ctx unsafe.Pointer)uint8{
 state:=(*nested)(frame(ctx))
 if state.pc==0{
  state.pc=1
  return host(ctx,func(){if goid()!=state.gid{panic("structured child changed host")};runtime.GC()})
 }
 state.done=true;return 3
}
type root struct{pc int;gid uint64;p *pressure;children []*child;nested *nested;done bool}
func rootResume(ctx unsafe.Pointer)uint8{
 state:=(*root)(frame(ctx))
 switch state.pc{
 case 0:
  for range 32{
   payload:=new([4096]byte);payload[4095]=37
   child:=&child{p:state.p,payload:payload};state.children=append(state.children,child)
   spawn(ctx,unsafe.Pointer(child),childResume)
  }
  state.pc=1;return 1
 case 1:
  state.pc=2
  return host(ctx,func(){
   if goid()!=state.gid{panic("public root did not retain original caller")}
   p:=state.p;p.condition.L.Lock()
   for p.ready!=32{p.condition.Wait()}
   runtime.GC();p.release=true;p.condition.Broadcast()
   for p.done!=32{p.condition.Wait()}
   p.condition.L.Unlock()
  })
 case 2:
  state.pc=3;state.nested=&nested{gid:state.gid}
  await(ctx,unsafe.Pointer(state.nested),nestedResume);return 2
 case 3:
  if !state.nested.done||goid()!=state.gid{panic("structured return lost host")}
  state.done=true;return 3
 }
 panic("invalid ordinary host root state")
}
func main(){
 p:=&pressure{condition:sync.NewCond(new(sync.Mutex))}
 state:=&root{gid:goid(),p:p}
 ordinaryRootDriver(unsafe.Pointer(state),rootResume,0)
 if !state.done||p.ready!=32||p.done!=32{panic("ordinary host pressure incomplete")}
 runtime.GC()
 for _,child:=range state.children{if child.payload[4095]!=37{panic("returned opaque payload lost")}}
 println("callable-host-affinity-ok")
}
`
	testCallableProgramme(t, source, "callable-host-affinity-ok")
}

func TestCallableOrdinaryHostTerminals(t *testing.T) {
	const source = `package main
import("runtime";"unsafe")
//go:linkname host runtime.coroHostCall
func host(unsafe.Pointer,func())uint8
//go:linkname hostDefer runtime.coroHostDefer
func hostDefer(unsafe.Pointer,func(any,bool,bool))uint8
//go:linkname terminal runtime.coroTerminalAction
func terminal(unsafe.Pointer)uint8
//go:linkname frame runtime.coroFrame
func frame(unsafe.Pointer)unsafe.Pointer
//go:linkname run runtime.coroRunFrame
func run(unsafe.Pointer,func(unsafe.Pointer)uint8,uintptr)bool
var ordinaryRootDriver=run

type state struct{pc,mode,cleanups int;caught any;token *int;returned bool}
func inject(value any,panicking,exiting bool){
 if panicking{
  if exiting{defer func(){panic(value)}();runtime.Goexit()}
  panic(value)
 }
 if exiting{runtime.Goexit()}
}
//go:noinline
func indirect()any{return recover()}
func resume(ctx unsafe.Pointer)uint8{
 s:=(*state)(frame(ctx))
 switch s.pc{
 case 0:
  s.pc=1
  return host(ctx,func(){if s.mode==0||s.mode==3||s.mode==4{panic(s.token)};runtime.Goexit()})
 case 1:
  s.pc=2
  return hostDefer(ctx,func(value any,panicking,exiting bool){
   var target func()
   switch s.mode{
   case 0:target=func(){s.cleanups++;s.caught=recover()}
   case 2:target=func(){s.cleanups++;panic(s.token)}
   case 3:target=func(){s.cleanups++;runtime.Goexit()}
   case 4:target=func(){s.cleanups++;if indirect()!=nil{panic("indirect recover borrowed logical panic")}}
   default:target=func(){s.cleanups++;if recover()!=nil{panic("Goexit supplied panic")}}
   }
   defer target()
   inject(value,panicking,exiting)
  })
 case 2:
  s.pc=3
  return hostDefer(ctx,func(value any,panicking,exiting bool){
   target:=func(){
    s.cleanups++
    got:=recover()
    if s.mode==2{s.caught=got}else if s.mode!=4&&got!=nil{panic("unexpected cleanup panic")}
    if s.mode==4{s.caught=got}
   }
   defer target()
   inject(value,panicking,exiting)
  })
 case 3:
  s.pc=4
  return hostDefer(ctx,func(value any,panicking,exiting bool){
   target:=func(){s.cleanups++;if recover()!=nil{panic("cleanup retained recovered panic")}}
   defer target()
   inject(value,panicking,exiting)
  })
 case 4:
  if action:=terminal(ctx);action!=0{return action}
  return 3
 }
 panic("invalid opaque terminal state")
}
func execute(mode int){
 done:=make(chan bool);token:=new(int);s:=&state{mode:mode,token:token}
 go func(){defer close(done);ordinaryRootDriver(unsafe.Pointer(s),resume,0);s.returned=true}()
 <-done
 if s.cleanups!=3{panic("opaque terminal cleanup count")}
 if mode==0||mode==2||mode==4{if s.caught!=token{panic("opaque direct recover value")}}
 wantReturn:=mode==0||mode==4
 if s.returned!=wantReturn{panic("opaque Goexit returned or recovered panic failed to return")}
 runtime.GC()
}
func main(){for mode:=0;mode<5;mode++{execute(mode)};println("callable-host-terminals-ok")}
`
	testCallableProgramme(t, source, "callable-host-terminals-ok")
}
