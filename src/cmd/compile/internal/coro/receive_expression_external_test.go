// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestReceiveExpressions(t *testing.T) {
	const source = `package main
import "runtime"
var trace int
func effect() int {trace=trace*10+1;return 3}
func cleanup() {trace=trace*10+2}
func plain(v int) int {return v+7}
func receive(c chan int) int {return <-c}
func add(c chan int) int {return <-c+1}
func argument(c chan int) int {return plain(<-c)}
func pair(c chan int) (int,int) {return <-c,<-c}
func condition(c chan int) int {if <-c==9 {return 10};return 0}
func after(c chan int) int {return <-c+effect()}
func withCleanup(c chan int) int {defer cleanup();return <-c}
func literal(c chan int) int {return func(in chan int)int{return <-in}(c)}
type node struct {value int; child *node}
func pointer(c chan *node) *node {return <-c}
func nested(c chan chan *node) *node {return <-(<-c)}
func conditional(b bool,c chan bool) bool {return b && <-c}
func prior(c chan int) int {return effect()+<-c}
func loop(c chan bool) int {n:=0;for <-c {n++};return n}
func main() {
 c:=make(chan int,16);for _,v:=range []int{3,4,5,6,7,9,8,11,12,13} {c<-v}
 if receive(c)!=3 || add(c)!=5 || argument(c)!=12 {panic("receive value")}
 a,b:=pair(c);if a!=6 || b!=7 || condition(c)!=10 {panic("receive order")}
 trace=0;if after(c)!=11 || trace!=1 {panic("trailing operation")}
 trace=0;if withCleanup(c)!=11 || trace!=2 {panic("defer cleanup")}
 if literal(c)!=12 {panic("literal return")}
 trace=0;if prior(c)!=16 || trace!=1 {panic("prior fallback")}
 if conditional(false,nil) {panic("short circuit")}
 flags:=make(chan bool,2);flags<-true;flags<-false;if loop(flags)!=1 {panic("loop header")}
 ptr:=make(chan *node,1);value:=&node{41,&node{value:42}};ptr<-value
 got:=pointer(ptr);runtime.GC();if got.value!=41 || got.child.value!=42 {panic("pointer root")}
 ptr<-got;outer:=make(chan chan *node,1);outer<-ptr
 if nested(outer)!=got {panic("nested receive")}
 close(c);if receive(c)!=0 {panic("closed receive")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, []switchFallback{
		{"prior", "channel site 1: nested receive in return"},
	}, []string{"receive", "add", "argument", "pair", "condition", "after", "withCleanup", "literal", "pointer", "nested", "conditional", "loop"})
}
