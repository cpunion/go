// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// Check snapshots after parameter mutation, repeated reads, deep serial
// recursion, independent public go factories with nonnil ctx, and GC while
// every lineage is suspended. All variants also run through ordinary Go.
func TestRootArrayLineage(t *testing.T) {
	const source = `package main
import "runtime"
//go:noinline
func read(p *[5120]byte) int { return int(p[0])+int(p[5118])+int(p[5119]) }
func sequence(n,seed int) int {
 var data [5120]byte
 data[5119]=byte(seed)
 seed += 7
 if n==0 {runtime.Gosched();return read(&data)+read(&data)}
 got:=sequence(n-1,seed)
 return got+read(&data)+read(&data)
}
var collect=make(chan bool)
var collected=make(chan bool)
func gcLoop(){for range collect{runtime.GC();collected<-true}}
func independent(n,seed int,done chan int,root bool) int {
 var data [5120]byte
 data[5119]=byte(seed)
 seed+=7
 got:=0
 if n==0 {runtime.Gosched();collect<-true;<-collected} else {got=independent(n-1,seed,done,false)}
 got+=read(&data)+read(&data)
 if root {done<-got}
 return got
}
func spawn(done chan int,n,seed int){go independent(n,seed,done,true);runtime.Gosched()}
func want(n,seed int)int{v:=0;for i:=0;i<=n;i++{v+=2*int(byte(seed+7*i))};return v}
func main(){
 go gcLoop()
 runtime.LockOSThread()
 if sequence(64,19)!=want(64,19){panic("locked-thread ordinary entry")}
 runtime.UnlockOSThread()
 for _,n:=range []int{0,1,16,64,513} {
  for seed:=0;seed<4;seed++ {if sequence(n,seed)!=want(n,seed){panic("serial")}}
  done:=make(chan int,8)
  for seed:=0;seed<8;seed++ {spawn(done,n,seed)}
  total:=0;expected:=0
  for seed:=0;seed<8;seed++ {total+=<-done;expected+=want(n,seed)}
  if total!=expected {panic("independent storage")}
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"sequence", "independent"})
}
