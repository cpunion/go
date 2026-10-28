// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// A public task boundary must start a fresh array root, including when the
// caller is native and when several callers concurrently enter the same leaf.
func TestRootArrayCallerBoundary(t *testing.T) {
	const source = `package main
import "runtime"
//go:noinline
func read(p *[5120]byte) int { return int(p[0])+int(p[5119]) }
func recursive(n,seed int,ready,release chan bool) int {
 var data [5120]byte
 data[5119]=byte(seed)
 seed+=7
 if n==0 {ready<-true;<-release;runtime.Gosched();return read(&data)+read(&data)}
 got:=recursive(n-1,seed,ready,release)
 return got+read(&data)+read(&data)
}
func left(n,seed int,ready,release chan bool) int {
 a:=recursive(n,seed,ready,release)
 b:=recursive(n,seed+3,ready,release)
 runtime.Gosched()
 return a+b
}
func right(n,seed int,ready,release chan bool) int {
 a:=recursive(n,seed+5,ready,release)
 b:=left(n,seed,ready,release)
 return a+b
}
func driver(which,n,seed int,ready,release chan bool,done chan int) {
 if which==0 {done<-left(n,seed,ready,release)} else {done<-right(n,seed,ready,release)}
}
func collect(ready,release chan bool) {for range ready{runtime.GC();release<-true}}
func want(n,seed int)int{v:=0;for i:=0;i<=n;i++{v+=2*int(byte(seed+7*i))};return v}
func expected(which,n,seed int)int {
 v:=want(n,seed)+want(n,seed+3)
 if which!=0 {v+=want(n,seed+5)}
 return v
}
func abort(n int,closed chan int) int {
 var data [5120]byte
 data[5119]=byte(n)
 if n==0 {closed<-1;return read(&data)}
 got:=abort(n-1,closed)
 return got+read(&data)
}
func abortCaller(n int,closed chan int)int {v:=abort(n,closed);runtime.Gosched();return v}
func recovered(n int,closed chan int)(caught bool) {
 for range 1 {
  defer func(){v:=recover();if v==nil {panic("missing panic")};caught=true}()
  abortCaller(n,closed)
 }
 return
}
func main(){
 ready,release:=make(chan bool),make(chan bool)
 go collect(ready,release)
 runtime.LockOSThread()
 if left(16,19,ready,release)!=expected(0,16,19){panic("ordinary locked caller")}
 runtime.UnlockOSThread()
 for _,n:=range []int{0,1,16,64,513} {
  for seed:=0;seed<3;seed++ {
   if left(n,seed,ready,release)!=expected(0,n,seed){panic("left")}
   if right(n,seed,ready,release)!=expected(1,n,seed){panic("right")}
  }
  done:=make(chan int,4)
  expectedTotal:=0
  for seed:=0;seed<4;seed++ {go driver(seed%2,n,seed,ready,release,done);expectedTotal+=expected(seed%2,n,seed)}
  total:=0;for range 4 {total+=<-done}
  if total!=expectedTotal{panic("independent concurrent caller roots")}
 }
 closed:=make(chan int);close(closed)
 for _,n:=range []int{0,16,513} {if !recovered(n,closed){panic("terminal cleanup")}}
 if right(513,19,ready,release)!=expected(1,513,19){panic("reuse after panic")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"recursive", "left", "right", "abort", "abortCaller"})
}
