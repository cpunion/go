// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestNativeParkedCacheGC(t *testing.T) {
	const source = `package main
import("fmt";"runtime";"runtime/debug";"time")
//go:noinline
func value(p *[5120]byte) int{return int(p[0]&1)}
func recursive(n int) int {
 var data [5120]byte
 data[0]=byte(n)
 if n==0 {runtime.Gosched();return 1+value(&data)}
 return recursive(n-1)+1+value(&data)
}
func live(n,mode int,ready,release chan struct{}) int {
 var pointers [128]*int
 pointers[0]=new(int)
 *pointers[0]=n+1
 v:=0
 if n==0 {
  ready<-struct{}{}; <-release
  if mode==2 {panic("wake-panic")}
  if mode==3 {runtime.Goexit()}
 } else {v=live(n-1,mode,ready,release)}
 for i:=1;i<len(pointers);i++ {if pointers[i]!=nil {panic("stale pointer")}}
 if *pointers[0]!=n+1 {panic("lost live pointer")}
 return v+*pointers[0]
}
func parked(mode int,ready,release chan struct{}) int {
 v:=recursive(512)
 if mode==0 {ready<-struct{}{};<-release;return v}
 return v+live(37,mode,ready,release)
}
type observer interface{touch()}
type noop struct{}
func(noop)touch(){}
func driver(o observer,mode int,ready,release chan struct{},done chan int){
 o.touch()
 returned:=false
 defer func(){
  v:=recover()
  if mode==2 && v=="wake-panic" {done<-111;return}
  if mode==3 && v==nil && !returned {done<-222;return}
  if v!=nil {panic(v)}
 }()
 v:=parked(mode,ready,release)
 returned=true
 done<-v
}
func main(){
 debug.SetGCPercent(-1)
 for mode:=0;mode<4;mode++ {
  var before,after runtime.MemStats
  runtime.GC();runtime.GC();runtime.ReadMemStats(&before)
  ready,release,done:=make(chan struct{},8),make(chan struct{}),make(chan int,8)
  for i:=0;i<8;i++ {go driver(noop{},mode,ready,release,done)}
  for i:=0;i<8;i++ {<-ready}
  time.Sleep(10*time.Millisecond)
  runtime.GC();runtime.GC();runtime.ReadMemStats(&after)
  limit:=int64(1<<20)
  if mode!=0 {limit=2<<20}
  retained:=int64(after.HeapAlloc)-int64(before.HeapAlloc)
  if retained>limit {panic(fmt.Sprintf("parked cache retained %d heap bytes, mode=%d",retained,mode))}
  close(release)
  want:=769
  if mode==1 {want+=741}
  if mode==2 {want=111}
  if mode==3 {want=222}
  for i:=0;i<8;i++ {if got:=<-done;got!=want {panic(fmt.Sprintf("outcome=%d want=%d mode=%d",got,want,mode))}}
 }
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"recursive", "live", "parked"})
}
