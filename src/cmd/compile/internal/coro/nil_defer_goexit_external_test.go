// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestOpenNilDeferGoexit(t *testing.T) {
	const source = `package main
import "runtime"
var caught int
func nativeExit(f func(), done chan struct{}) {
 defer close(done)
 defer func() {
  if _,ok:=recover().(runtime.Error);!ok {panic("missing nil-call runtime.Error")}
  caught++
 }()
 defer f()
 runtime.Gosched()
 runtime.Goexit()
 panic("Goexit returned")
}
func main() {
 for i:=0;i<64;i++ {
  done:=make(chan struct{})
  go nativeExit(nil,done)
  <-done
  if caught!=i+1 {panic("cleanup count")}
  runtime.GC()
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"nativeExit"}, "nativeExit")
}
