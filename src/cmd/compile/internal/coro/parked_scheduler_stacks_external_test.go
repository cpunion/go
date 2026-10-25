// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestNativeParkedSchedulerStacks(t *testing.T) {
	const source = `package main
import("runtime";"time")
func yieldPark(value int,ready chan struct{},release chan struct{})int{
 payload:=make([]byte,4096+value%2)
 payload[0]=byte(value)
 runtime.Gosched()
 ready<-struct{}{}
 <-release
 if payload[0]!=byte(value){panic("parked payload lost")}
 return value
}
type observer interface{touch()}
type noop struct{}
func(noop)touch(){}
func driver(o observer,value int,ready,release chan struct{},done chan int){
 o.touch()
 done<-yieldPark(value,ready,release)
}
func main(){
 runtime.GOMAXPROCS(1)
 for round:=0;round<3;round++{
  var before,parked runtime.MemStats
  runtime.GC();runtime.ReadMemStats(&before)
  ready,release,done:=make(chan struct{},64),make(chan struct{}),make(chan int,64)
  for i:=0;i<64;i++{go driver(noop{},i,ready,release,done)}
  for i:=0;i<64;i++{<-ready}
  time.Sleep(time.Millisecond)
  runtime.GC();runtime.GC();runtime.ReadMemStats(&parked)
  if parked.StackInuse>before.StackInuse+512<<10{
   println("Go stack bytes",before.StackInuse,parked.StackInuse)
   panic("parked roots retained per-context scheduler stacks")
  }
  close(release)
  sum:=0
  for i:=0;i<64;i++{sum+=<-done}
  if sum!=63*64/2{panic("invalid resumed roots")}
 }
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"yieldPark"})
}
