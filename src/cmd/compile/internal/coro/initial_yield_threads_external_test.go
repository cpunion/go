// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestNativeInitialYieldThreads(t *testing.T) {
	const source = `package main
import("runtime";"time")
func yieldThenWait(ready,release chan struct{})int{
 runtime.Gosched()
 ready<-struct{}{}
 <-release
 return 7
}
type observer interface{touch()}
type noop struct{}
func(noop)touch(){}
func driver(o observer,ready,release chan struct{},done chan int){
 o.touch()
 value:=yieldThenWait(ready,release)
 done<-value
}
func main(){
 runtime.GOMAXPROCS(1)
 for round:=0;round<3;round++{
  before,_:=runtime.ThreadCreateProfile(nil)
  ready,release,done:=make(chan struct{},64),make(chan struct{}),make(chan int,64)
  for i:=0;i<64;i++{go driver(noop{},ready,release,done)}
  for i:=0;i<64;i++{<-ready}
  time.Sleep(time.Millisecond)
  after,_:=runtime.ThreadCreateProfile(nil)
  if after-before>8{panic("initial yields created one thread per root")}
  runtime.GC();runtime.GC()
  close(release)
  for i:=0;i<64;i++{if got:=<-done;got!=7{panic(got)}}
 }
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"yieldThenWait"})
}
