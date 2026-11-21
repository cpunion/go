// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestNativeChannelPairWake(t *testing.T) {
	const source = `package main
import "runtime"

func worker(ping <-chan *int, pong chan<- *int, n int) {
 for i := 0; i < n; i++ {
  item := <-ping
  pong <- item
 }
}
func exchange(ping, pong chan *int, item *int, n int) {
 go worker(ping, pong, n)
 for i := 0; i < n; i++ {
  ping <- item
  got := <-pong
  if got != item { panic("channel transfer") }
 }
}
func sendTwice(first, second chan *int, item *int) {
 first <- item
 second <- item
}
func forward(first <-chan *int, seen chan<- *int) {
 got := <-first
 seen <- got
}
func closedTask(first, second, seen chan *int, item *int) {
 go forward(first, seen)
 defer func() {
  if _, ok := recover().(runtime.Error); !ok { panic("missing closed-channel panic") }
 }()
 runtime.Gosched()
 first <- item
 second <- item
}
func handoff(first chan *int, buffered chan *int, item *int, seen chan *int) {
 go forward(first, seen)
 runtime.Gosched()
 sendTwice(first, buffered, item)
 if <-seen != item || <-buffered != item { panic("ready second send") }
}
func main() {
 for i := 0; i < 5; i++ {
  item := new(int); *item = 97
  exchange(make(chan *int), make(chan *int), item, 2000)
  handoff(make(chan *int), make(chan *int, 1), item, make(chan *int))
  first, closed, seen := make(chan *int), make(chan *int), make(chan *int,1)
  close(closed)
  closedTask(first, closed, seen, item)
  runtime.GC()
  if got := <-seen; got != item || *got != 97 { panic("lost peer after panic") }
 }
 println("switch-return-ok")
}
`
	required := []string{"worker", "exchange", "sendTwice", "forward", "closedTask"}
	testSwitchReturnProgram(t, source, nil, required, required...)
}
