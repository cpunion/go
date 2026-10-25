// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// A completed recursive burst must not remain cached for the lifetime of each
// parked public root. With eight roots an untrimmed4MiB-per-context policy
// retains more than20MiB here, even after repeated collections.
func TestNativeBurstCacheParkedRoots(t *testing.T) {
	const source = `package main
import (
 "fmt"
 "runtime"
 "runtime/debug"
 "time"
)
//go:noinline
func value(p *[5120]byte) int {return int(p[0]&1)}
func recursive(n int) int {
 var data [5120]byte
 data[0]=byte(n)
 if n==0 {runtime.Gosched();return 1+value(&data)}
 return recursive(n-1)+1+value(&data)
}
func parked(ready,release chan struct{}) int {
 v:=recursive(512)
 ready<-struct{}{}
 <-release
 return v
}
type observer interface{touch()}
type noop struct{}
func(noop)touch(){}
func driver(o observer,ready,release chan struct{},done chan int){
 // Keep the goroutine entry ordinary; each parked call owns a public root.
 o.touch()
 for range 1 {done<-parked(ready,release)}
}
func main(){
 debug.SetGCPercent(-1)
 var before,after runtime.MemStats
 runtime.GC();runtime.GC();runtime.ReadMemStats(&before)
 ready,release,done:=make(chan struct{},8),make(chan struct{}),make(chan int,8)
 for i:=0;i<8;i++ {go driver(noop{},ready,release,done)}
 for i:=0;i<8;i++ {<-ready}
 time.Sleep(10*time.Millisecond)
 runtime.GC();runtime.GC();runtime.ReadMemStats(&after)
 retained:=int64(after.HeapAlloc)-int64(before.HeapAlloc)
 if retained>8<<20 {panic(fmt.Sprintf("completed bursts retained %d heap bytes in parked roots",retained))}
 close(release)
 for i:=0;i<8;i++ {if got:=<-done;got!=769 {panic("recursive result")}}
 runtime.GC();runtime.GC()
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"recursive", "parked"})
}
