// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// Pair the source admission tests with a runtime ABI probe that cannot pass by
// accidentally choosing the ordinary path for every native-capable function.
func TestCallableCompanionDeferInvocation(t *testing.T) {
	const source = `package main
import("reflect";"runtime";"unsafe")
//go:linkname lookup runtime.coroLookupCallable
func lookup(uintptr,unsafe.Pointer)unsafe.Pointer
//go:linkname start runtime.coroDeferStartFunction
func start(unsafe.Pointer,any,unsafe.Pointer,func(any,bool,bool))
//go:linkname token runtime.coroDeferToken
func token(unsafe.Pointer)unsafe.Pointer
//go:linkname action runtime.coroDeferAction
func action(unsafe.Pointer)uint8
//go:linkname panicking runtime.coroPanic
func panicking(unsafe.Pointer,any)
//go:linkname recoverOwner runtime.coroDeferRecover
func recoverOwner(unsafe.Pointer)any
//go:linkname inject runtime.coroDeferInject
func inject(any,bool,bool)
//go:linkname frame runtime.coroFrame
func frame(unsafe.Pointer)unsafe.Pointer
//go:linkname run runtime.coroRunFrame
func run(unsafe.Pointer,func(unsafe.Pointer)uint8,uintptr)bool
var ordinaryRootDriver=run
func native(dst *any){runtime.Gosched();runtime.GC();*dst=recover()}
func nonSuspending(dst *any,replacement any){*dst=recover();panic(replacement)}
func plain(dst *any){*dst=recover()}
func normal(f func(*any),dst *any){f(dst);runtime.Gosched()}
func ordinaryCall(dst *any){normal(plain,dst);runtime.Gosched()}
func signature(f any)unsafe.Pointer{return (*[2]unsafe.Pointer)(unsafe.Pointer(&f))[0]}
func noFallback(any,bool,bool){panic("native direct-defer capability was bypassed")}
type state struct{pc int;incoming,replacement,observed any;fallbacks int;done bool}
func drive(ctx unsafe.Pointer)uint8{
 s:=(*state)(frame(ctx))
 switch s.pc{
 case 0:
  s.pc=1;panicking(ctx,s.incoming)
  request:=struct{dst *any}{&s.observed}
  start(token(ctx),native,unsafe.Pointer(&request),noFallback)
  if action(ctx)!=2{panic("native cleanup did not await its own task")};return 2
 case 1:
  if s.observed!=s.incoming||recoverOwner(token(ctx))!=nil{panic("suspending direct recover ownership")}
  s.observed=nil;s.pc=2;panicking(ctx,s.incoming)
  request:=struct{dst *any;replacement any}{&s.observed,s.replacement}
  start(token(ctx),nonSuspending,unsafe.Pointer(&request),noFallback)
  if action(ctx)!=2{panic("non-suspending cleanup used ordinary host")};return 2
 case 2:
  if s.observed!=s.incoming||recoverOwner(token(ctx))!=s.replacement{panic("non-suspending recover and replacement ownership")}
  s.observed=nil;s.pc=3;panicking(ctx,s.incoming)
  request:=struct{dst *any}{&s.observed}
  start(token(ctx),ordinaryCall,unsafe.Pointer(&request),noFallback)
  if action(ctx)!=2{panic("normal-call cleanup lost native task")};return 2
 case 3:
  if s.observed!=nil||recoverOwner(token(ctx))!=s.incoming{panic("ordinary call borrowed direct-defer recovery")}
  s.pc=4;panicking(ctx,s.incoming)
  request:=struct{dst *any}{&s.observed}
  start(token(ctx),plain,unsafe.Pointer(&request),func(value any,panicking,exiting bool){s.fallbacks++;defer plain(&s.observed);inject(value,panicking,exiting)})
  if action(ctx)!=8||s.fallbacks!=0{panic("ordinary direct-defer handoff barrier")};return 8
 case 4:
  if s.fallbacks!=1||s.observed!=s.incoming||recoverOwner(token(ctx))!=nil{panic("ordinary direct defer did not recover its own panic")}
  s.done=true;return 3
 }
 panic("invalid direct-defer ABI driver")
}
func main(){
 var observed any;normal(plain,&observed);if observed!=nil{panic("ordinary normal call recovered")}
 p:=lookup(reflect.ValueOf(plain).Pointer(),signature(plain));if p==nil{panic("missing demanded normal-call companion")}
 words:=(*[6]uintptr)(p);if words[4]!=0||words[5]!=0{panic("normal-recover adapter acquired direct-defer capability")}
 s:=&state{incoming:new(int),replacement:new(int)}
 ordinaryRootDriver(unsafe.Pointer(s),drive,0)
 if !s.done{panic("defer ABI driver incomplete")}
 println("callable-defer-invocation-ok")
}
`
	testCallableProgramme(t, source, "callable-defer-invocation-ok")
}
