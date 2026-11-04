// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// Generic wrappers retain their dictionary and receiver layout on the ordinary
// fallback path while their open callers continue in native activations.
func TestOpenGenericFallback(t *testing.T) {
	const source = `package main
import("runtime";"unsafe")
//go:linkname bodyless runtime.nanotime
func bodyless()int64
type payload struct{p *int;values []int;labels map[string]int}
func generic[T any](v T)T{runtime.Gosched();runtime.GC();return any(v).(T)}
func genericSend[T any](v T,done chan T){runtime.Gosched();runtime.GC();done<-any(v).(T)}
func genericRecover[T any](p *any){*p=recover();runtime.GC();runtime.Gosched()}
func genericExit[T any](done chan struct{}){defer close(done);runtime.Gosched();runtime.Goexit()}
type box[T any] struct{value T;observed *any}
func(b box[T])Read()T{runtime.Gosched();runtime.GC();return any(b.value).(T)}
func(b box[T])Capture(){*b.observed=recover();runtime.Gosched();runtime.GC()}
func(b box[T])Send(done chan T){runtime.Gosched();done<-any(b.value).(T)}
type reader interface{Read()payload;Capture();Send(chan payload)}
func call(f func(payload)payload,v payload)payload{runtime.Gosched();return f(v)}
func launch(f func(payload,chan payload),v payload,done chan payload){runtime.Gosched();go f(v,done)}
func cleanup(f func(*any),p *any,token any){defer f(p);runtime.Gosched();panic(token)}
func launchExit(f func(chan struct{}),done chan struct{}){runtime.Gosched();go f(done)}
func methodCall(v reader)payload{runtime.Gosched();return v.Read()}
func methodGo(v reader,done chan payload){runtime.Gosched();go v.Send(done)}
func methodDefer(v reader,token any){defer v.Capture();runtime.Gosched();panic(token)}
func boundCall(f func()payload)payload{runtime.Gosched();return f()}
func boundGo(f func(chan payload),done chan payload){runtime.Gosched();go f(done)}
func boundDefer(f func(),token any){defer f();runtime.Gosched();panic(token)}
func unavailable(f func()int64){runtime.Gosched();if f()<=0{panic("bodyless result")}}
func unavailableDefer(f func()int64){defer f();runtime.Gosched()}
func check(v payload){if *v.p!=13||len(v.values)!=2||v.values[1]!=17||v.labels["x"]!=19{panic("dictionary or typed pointer result")}}
var _ unsafe.Pointer
func main(){
 p:=new(int);*p=13;v:=payload{p,[]int{11,17},map[string]int{"x":19}}
 check(call(generic[payload],v));done:=make(chan payload);launch(genericSend[payload],v,done);check(<-done)
 var observed any;token:=new(int);cleanup(genericRecover[payload],&observed,token);if observed!=token{panic("generic defer recover")}
 exit:=make(chan struct{});launchExit(genericExit[payload],exit);<-exit
 var r reader=box[payload]{v,&observed};check(methodCall(r));methodGo(r,done);check(<-done)
 observed=nil;methodDefer(r,token);if observed!=token{panic("generic interface defer recover")}
 check(boundCall(r.Read));boundGo(r.Send,done);check(<-done)
 observed=nil;boundDefer(r.Capture,token);if observed!=token{panic("generic bound defer recover")}
 concrete:=box[payload]{v,&observed};check(boundCall(concrete.Read));boundGo(concrete.Send,done);check(<-done)
 observed=nil;boundDefer(concrete.Capture,token);if observed!=token{panic("generic concrete bound recover")}
 unavailable(bodyless);unavailableDefer(bodyless)
 println("switch-return-ok")
}
`
	required := []string{"call", "launch", "cleanup", "launchExit", "methodCall", "methodGo", "methodDefer", "boundCall", "boundGo", "boundDefer", "unavailable", "unavailableDefer"}
	testSwitchReturnFilesNative(t, map[string]string{"main.go": source}, "", nil, required, required)
}
