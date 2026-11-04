// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestOpenInterfaceCalls(t *testing.T) {
	const source = `package main
import "runtime"
type worker interface{Add(int)(int,*box);Capture();Exit(chan struct{});Store(int,chan struct{})}
type box struct{n int;caught any}
func(p *box)Add(n int)(int,*box){runtime.Gosched();runtime.GC();if p==nil{return 71,nil};p.n+=n;return p.n,p}
func(p *box)Store(n int,done chan struct{}){runtime.Gosched();runtime.GC();p.n=n;close(done)}
func(p *box)Capture(){runtime.Gosched();runtime.GC();p.caught=recover()}
func(p *box)Exit(done chan struct{}){defer close(done);runtime.Gosched();runtime.Goexit();p.n=999}
func call(w worker,n int)(int,*box){runtime.Gosched();return w.Add(n)}
func launch(w worker,n int,done chan struct{}){runtime.Gosched();go w.Store(n,done)}
func cleanup(w worker,token any){defer w.Capture();runtime.Gosched();panic(token)}
func launchExit(w worker,done chan struct{}){runtime.Gosched();go w.Exit(done)}
func bound(f func(int)(int,*box),n int)(int,*box){runtime.Gosched();return f(n)}
func boundCleanup(f func(),token any){defer f();runtime.Gosched();panic(token)}
func arg(order *[]int,n int)int{*order=append(*order,n);return n}
func nilCall(w worker,order *[]int){w.Add(arg(order,1));*order=append(*order,2)}
func nilGo(w worker,order *[]int){go w.Store(arg(order,1),nil);*order=append(*order,2)}
func nilDefer(w worker,order *[]int){defer w.Store(arg(order,1),nil);*order=append(*order,2)}
func nilBound(w worker,order *[]int){f:=w.Add;bound(f,arg(order,1));*order=append(*order,2)}
func nilBoundOperand(w worker,order *[]int){bound(w.Add,arg(order,1));*order=append(*order,2)}
func caught(f func(worker,*[]int),order *[]int)(yes bool){defer func(){yes=recover()!=nil}();f(nil,order);return}
func main(){
 p:=&box{n:11};var w worker=p
 n,q:=call(w,3);if n!=14||q!=p{panic("interface tuple result")}
 f:=w.Add;w=&box{n:99};runtime.GC()
 n,q=bound(f,5);if n!=19||q!=p{panic("bound interface receiver snapshot")}
 done:=make(chan struct{});launch(p,23,done);<-done
 if p.n!=23{panic("interface go")}
 token:=new(int);cleanup(p,token);if p.caught!=token{panic("interface direct defer recover")}
 w=p;boundCleanup(w.Capture,token);if p.caught!=token{panic("bound interface direct recover")}
 w.Capture();if p.caught!=nil{panic("normal interface call borrowed recover")}
 var absent *box;n,q=call(absent,7);if n!=71||q!=nil{panic("typed nil interface receiver")}
 for _,f:=range []func(worker,*[]int){nilCall,nilGo,nilDefer}{
  var order []int;if !caught(f,&order)||len(order)!=1||order[0]!=1{panic("nil interface argument evaluation or caller panic timing")}
 }
 var order []int;if !caught(nilBound,&order)||len(order)!=0{panic("nil interface bound method evaluation timing")}
 order=nil;if !caught(nilBoundOperand,&order)||len(order)!=1||order[0]!=1{panic("nil interface bound operand evaluation timing")}
 done=make(chan struct{});launchExit(p,done);<-done;if p.n!=23{panic("interface Goexit returned")}
 println("switch-return-ok")
}
`
	required := []string{"call", "launch", "cleanup", "launchExit", "bound", "boundCleanup"}
	testSwitchReturnProgram(t, source, nil, required, required...)
}

// This probe requires actual interface factories and direct-defer child tasks;
// running the ordinary method as a fallback cannot satisfy its assertions.
func TestInterfaceCallableInvocation(t *testing.T) {
	const source = `package main
import("runtime";"unsafe")
//go:linkname lookup runtime.coroLookupInterface
func lookup(unsafe.Pointer,unsafe.Pointer,uintptr,any)(unsafe.Pointer,unsafe.Pointer)
//go:linkname lookupFunction runtime.coroLookupFunction
func lookupFunction(any)(unsafe.Pointer,unsafe.Pointer)
//go:linkname factory runtime.coroCallableFactory
func factory(unsafe.Pointer,unsafe.Pointer,unsafe.Pointer,unsafe.Pointer)(unsafe.Pointer,func(unsafe.Pointer)uint8)
//go:linkname await runtime.coroAwaitFrame
func await(unsafe.Pointer,unsafe.Pointer,func(unsafe.Pointer)uint8)
//go:linkname start runtime.coroDeferStartInterface
func start(unsafe.Pointer,unsafe.Pointer,unsafe.Pointer,uintptr,any,unsafe.Pointer,func(any,bool,bool))
//go:linkname startFunction runtime.coroDeferStartFunction
func startFunction(unsafe.Pointer,any,unsafe.Pointer,func(any,bool,bool))
//go:linkname token runtime.coroDeferToken
func token(unsafe.Pointer)unsafe.Pointer
//go:linkname action runtime.coroDeferAction
func action(unsafe.Pointer)uint8
//go:linkname panicking runtime.coroPanic
func panicking(unsafe.Pointer,any)
//go:linkname recoverOwner runtime.coroDeferRecover
func recoverOwner(unsafe.Pointer)any
//go:linkname frame runtime.coroFrame
func frame(unsafe.Pointer)unsafe.Pointer
//go:linkname run runtime.coroRunFrame
func run(unsafe.Pointer,func(unsafe.Pointer)uint8,uintptr)bool
var ordinaryRootDriver=run
type worker interface{Add(int)int;Capture()}
type box struct{n int;observed *any;pad[4095]byte}
func(p *box)Add(n int)int{runtime.Gosched();runtime.GC();return p.n+n+int(p.pad[4094])}
func(p *box)Capture(){runtime.Gosched();runtime.GC();*p.observed=recover()}
type value struct{n int;observed *any;offset int}
func(v value)Add(n int)int{runtime.Gosched();runtime.GC();return v.n+n+v.offset}
func(v value)Capture(){*v.observed=recover()}
type promoted struct{value}
type direct struct{observed *any}
func(v direct)Add(n int)int{return n+30}
func(v direct)Capture(){*v.observed=recover()}
func normal(w worker){w.Capture();runtime.Gosched()}
type state struct{pc int;w worker;observed *any;incoming any;result int;done bool}
func noFallback(any,bool,bool){panic("interface native defer bypassed")}
func drive(ctx unsafe.Pointer)uint8{
 s:=(*state)(frame(ctx));words:=(*[2]unsafe.Pointer)(unsafe.Pointer(&s.w))
 switch s.pc{
 case 0:
  descriptor,environment:=lookup(words[0],words[1],0,(func(int)int)(nil))
  if descriptor==nil{panic("missing interface companion")}
  if p,_:=lookup(words[0],words[1],0,(func()int)(nil));p!=nil{panic("interface signature mismatch accepted")}
  if p,_:=lookup(words[0],words[1],2,(func(int)int)(nil));p!=nil{panic("interface method bounds unchecked")}
  request:=struct{n int;result *int}{7,&s.result}
  child,resume:=factory(ctx,descriptor,environment,unsafe.Pointer(&request))
  s.pc=1;await(ctx,child,resume);return 2
 case 1:
  if s.result!=37{panic("interface receiver or result")}
  descriptor,environment:=lookupFunction(s.w.Add)
  if descriptor==nil{panic("missing bound interface companion")}
  request:=struct{n int;result *int}{11,&s.result}
  child,resume:=factory(ctx,descriptor,environment,unsafe.Pointer(&request))
  s.pc=2;await(ctx,child,resume);return 2
 case 2:
  if s.result!=41{panic("bound interface receiver or result")}
  s.pc=3;panicking(ctx,s.incoming)
  start(token(ctx),words[0],words[1],1,(func())(nil),nil,noFallback)
  if action(ctx)!=2{panic("interface cleanup used host")};return 2
 case 3:
  if *s.observed!=s.incoming||recoverOwner(token(ctx))!=nil{panic("interface lost direct recover")}
  s.pc=4;panicking(ctx,s.incoming)
  startFunction(token(ctx),s.w.Capture,nil,noFallback)
  if action(ctx)!=2{panic("bound interface cleanup used host")};return 2
 case 4:
  if *s.observed!=s.incoming||recoverOwner(token(ctx))!=nil{panic("bound interface lost direct recover")}
  *s.observed=nil;s.pc=5;panicking(ctx,s.incoming)
  request:=struct{w worker}{s.w}
  startFunction(token(ctx),normal,unsafe.Pointer(&request),noFallback)
  if action(ctx)!=2{panic("normal interface cleanup used host")};return 2
 case 5:
  if *s.observed!=nil||recoverOwner(token(ctx))!=s.incoming{panic("normal interface call borrowed direct recover")}
  s.done=true;return 3
 }
 panic("invalid interface driver")
}
func main(){
 var observed any
 p:=&box{n:13,observed:&observed};p.pad[4094]=17
 v:=value{13,&observed,17}
 for _,w:=range []worker{p,v,&v,promoted{v},&promoted{v},direct{&observed}}{
  observed=nil;s:=&state{w:w,observed:&observed,incoming:new(int)};runtime.GC()
  ordinaryRootDriver(unsafe.Pointer(s),drive,0)
  if !s.done{panic("interface driver incomplete")}
 }
 var absent *value;var w worker=absent;words:=(*[2]unsafe.Pointer)(unsafe.Pointer(&w))
 if p,_:=lookup(words[0],words[1],0,(func(int)int)(nil));p!=nil{panic("nil value wrapper bypassed panicwrap")}
 if p,_:=lookupFunction(w.Capture);p!=nil{panic("nil bound value wrapper bypassed panicwrap")}
 var absentPromoted *promoted;w=absentPromoted;words=(*[2]unsafe.Pointer)(unsafe.Pointer(&w))
 if p,_:=lookup(words[0],words[1],0,(func(int)int)(nil));p!=nil{panic("nil promoted receiver admitted before child startup")}
 w=direct{};words=(*[2]unsafe.Pointer)(unsafe.Pointer(&w))
 if p,_:=lookup(words[0],words[1],0,(func(int)int)(nil));p==nil{panic("nil direct-interface data rejected")}

 println("interface-invocation-ok")
}
`
	testCallableProgramme(t, source, "interface-invocation-ok")
}

func TestInterfaceNilValueReceiver(t *testing.T) {
	const source = `package main
import "runtime"
type receiver struct{n int}
func(v receiver)Run(){runtime.Gosched()}
type runner interface{Run()}
func call(v runner){runtime.Gosched();v.Run()}
func cleanup(v runner){defer v.Run();runtime.Gosched()}
func bound(f func()){runtime.Gosched();f()}
func caught(f func())(out any){defer func(){out=recover()}();f();return}
func main(){
 var p *receiver;var v runner=p
 for _,f:=range []func(){func(){call(v)},func(){cleanup(v)},func(){bound(v.Run)}}{
  got:=caught(f);if got==nil{panic("nil value receiver did not panic")}
  message:=got.(interface{Error()string}).Error()
  if message!="value method main.receiver.Run called using nil *receiver pointer"{panic(message)}
 }
 println("switch-return-ok")
}
`
	required := []string{"call", "cleanup", "bound"}
	testSwitchReturnProgram(t, source, nil, required, required...)
}
