// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestPublicChannelEntry(t *testing.T) {
	const source = `package main
import ("runtime"; "sync/atomic")

var phase atomic.Int32

//go:noinline
func channel(c chan *int) chan *int {
 phase.Add(1)
 return c
}
//go:noinline
func value(v *int) *int {
 phase.Store(phase.Load()*10+2)
 return v
}
//go:noinline
func send(c chan *int, v *int) {
 channel(c) <- value(v)
 phase.Add(100)
}
//go:noinline
func receive(c chan *int) *int { return <-channel(c) }
type received bool
//go:noinline
func pair(c chan *int) (*int, received) {
 v, ok := <-channel(c)
 return v, received(ok)
}
//go:noinline
func discard(c chan *int) { <-channel(c) }

func waitForPhase(want int32) {
 for phase.Load()==0 || phase.Load()==1 && want==12 {runtime.Gosched()}
 if phase.Load()!=want {panic("operands or suffix ran before the channel was ready")}
 runtime.GC()
}

func main() {
 goto body
body:
 a,b:=7,9
 full:=make(chan *int,1)
 send(full,&a)
 if phase.Load()!=112 || <-full!=&a {panic("buffered send")}
 phase.Store(0)
 full<-&a
 if receive(full)!=&a || phase.Load()!=1 {panic("buffered receive")}

 phase.Store(0)
 full<-&a
 done:=make(chan struct{})
 go func(){goto body;body:send(full,&b);close(done)}()
 waitForPhase(12)
 if <-full!=&a || <-full!=&b {panic("blocked send order")}
 <-done
 if phase.Load()!=112 {panic("blocked send operands repeated")}

 phase.Store(0)
 rendezvous:=make(chan *int)
 result:=make(chan *int,1)
 go func(){goto body;body:result<-receive(rendezvous)}()
 waitForPhase(1)
 rendezvous<-&b
 if <-result!=&b || phase.Load()!=1 {panic("blocked receive operands repeated")}

 phase.Store(0)
 full<-&b
 v,ok:=pair(full)
 if !ok || v!=&b || phase.Load()!=1 {panic("buffered comma-ok")}
 phase.Store(0)
 checked:=make(chan received,1)
 go func(){goto body;body:v,ok:=pair(rendezvous);result<-v;checked<-ok}()
 waitForPhase(1)
 rendezvous<-&a
 if <-result!=&a || !<-checked || phase.Load()!=1 {panic("blocked comma-ok")}
 close(full)
 phase.Store(0)
 v,ok=pair(full)
 if ok || v!=nil || phase.Load()!=1 {panic("closed comma-ok")}
 discard(full)
 if phase.Load()!=2 {panic("discard")}

 phase.Store(0)
 func(){
  goto body
 body:
  defer func(){if _,ok:=recover().(runtime.Error); !ok || phase.Load()!=12 {panic("closed send")}}()
  send(full,&a)
 }()

 phase.Store(0)
 locked:=make(chan *int)
 go func(){waitForPhase(1);locked<-&a}()
 runtime.LockOSThread()
 if receive(locked)!=&a || phase.Load()!=1 {panic("thread-locked fallback")}
 runtime.UnlockOSThread()

 println("switch-return-ok")
}
`
	names := []string{"send", "receive", "pair", "discard"}
	testSwitchReturnProgram(t, source, nil, names, names...)
}
