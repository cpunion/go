// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestDynamicDefer(t *testing.T) {
	const source = `package main
import "runtime"

type box struct { n int }
type callback func(*box, int) int
var trace []int
var gcRequest=make(chan bool)
var gcDone=make(chan bool)
func collect() {for range gcRequest {runtime.GC();gcDone<-true}}
func first(p *box, n int) int {trace=append(trace,p.n+n);return n}
func second(p *box, n int) int {trace=append(trace,100+p.n+n);return n}
func choose(which bool) callback {runtime.Gosched();if which {return second};return first}
var evaluations int
func argument() int {runtime.Gosched();evaluations++;return evaluations}
func snapshot(which bool) {
 f:=first
 if which {f=second}
 alias:=callback(f)
 p,n:=&box{10},3
 defer alias(p,n)
 f,alias,p,n=second,second,&box{1000},1000
 runtime.Gosched()
}
func repeated() {
 for i:=0;i<64;i++ {
  f:=first
  if i%2!=0 {f=second}
  defer f(&box{i},argument())
 }
 gcRequest<-true
 <-gcDone
 runtime.Gosched()
}
func returned(which bool) {f:=choose(which);defer f(&box{4},argument());runtime.Gosched()}
func zeroA() {trace=append(trace,7)}
func zeroB() {trace=append(trace,8)}
func zero(which bool) {f:=zeroA;if which {f=zeroB};defer f();f=zeroB;runtime.Gosched()}
func variadicA(values ...int) {trace=append(trace,values[0]+values[1])}
func variadicB(values ...int) {trace=append(trace,100+values[0]+values[1])}
func variadic(which bool) {
 f:=variadicA;if which {f=variadicB}
 values:=[]int{2,3}
 defer f(values...)
 values[0]=4
 runtime.Gosched()
}
func nilCall(which bool) (result any) {
 defer func(){result=recover()}()
 f:=zeroA
 if which {f=nil}
 defer f()
 // A nil deferred function must be registered before this write and yield.
 evaluations++
 runtime.Gosched()
 return
}
func nilArgument(which bool) (result any) {
 defer func(){result=recover()}()
 f:=first;if which {f=nil}
 defer f(&box{1},argument())
 evaluations++
 runtime.Gosched()
 return
}
func repeatedNil() (result any) {
 defer func(){result=recover()}()
 for i:=0;i<4;i++ {f:=zeroA;if i==2 {f=nil};defer f()}
 runtime.Gosched()
 return
}
func nilReplacesPanic(which bool) (result any) {
 defer func(){result=recover()}()
 f:=zeroA;if which {f=nil};defer f()
 runtime.Gosched()
 panic("original")
}
var exitMarker int
func nilDuringExit(which bool, done chan struct{}) {
 defer func(){close(done)}()
 defer func(){if recover()==nil {panic("missing nil panic during Goexit")};exitMarker++}()
 f:=zeroA;if which {f=nil};defer f()
 runtime.Gosched()
 runtime.Goexit()
 exitMarker+=100
}
func panicArgument() int {runtime.Gosched();panic("argument")}
func argumentPanic(which bool) (result any) {
 defer func(){result=recover()}()
 f:=first;if which {f=second}
 defer f(&box{1},panicArgument())
 return
}
func implicit(values []int) {trace=append(trace,values[0])}
func implicitOther(values []int) {trace=append(trace,values[1])}
func implicitPanic(which bool) (result any) {
 defer func(){result=recover()}()
 f:=implicit;if which {f=implicitOther}
 defer f(nil)
 runtime.Gosched()
 return
}
func bodyPanic(which bool) (result any) {
 defer func(){result=recover()}()
 f:=zeroA;if which {f=zeroB}
 defer f()
 runtime.Gosched()
 panic("body")
}
func exiting(which bool, done chan struct{}) {
 defer func(){close(done)}()
 f:=zeroA;if which {f=zeroB}
 defer f()
 runtime.Gosched()
 runtime.Goexit()
}
// A dynamic call in a source literal remains outside the operand-wrapper proof.
var global=zeroA
func open(f func()) {defer f();runtime.Gosched()}
func globalCall() {defer global();runtime.Gosched()}
func captured() {f:=zeroA;change:=func(){f=zeroB};change();defer f();runtime.Gosched()}
func addressed() {f:=zeroA;p:=&f;*p=zeroB;defer f();runtime.Gosched()}
func literal(f func()) {defer func(){f()}();runtime.Gosched()}
func yielding() {runtime.Gosched()}
func suspended(which bool) {f:=zeroA;if which {f=yielding};defer f();runtime.Gosched()}
func fails() {panic("target")}
func terminal(which bool) (result any) {
 defer func(){result=recover()}()
 f:=zeroA;if which {f=fails};defer f();runtime.Gosched();return
}
func recovering() {recover()}
func terminalRecover(which bool) {f:=zeroA;if which {f=recovering};defer f();runtime.Gosched()}
func leaving() {runtime.Goexit()}
func terminalExit(which bool, done chan struct{}) {defer func(){close(done)}();f:=zeroA;if which {f=leaving};defer f();runtime.Gosched()}
func equal(want ...int) {if len(trace)!=len(want) {panic("trace length")};for i,v:=range want {if trace[i]!=v {panic("trace order or snapshot")}};trace=nil}
func main() {
 go collect()
 snapshot(false);snapshot(true);equal(13,113)
 repeated()
 if evaluations!=64 || len(trace)!=64 {panic("repeated registration")}
 for j,v:=range trace {i:=63-j;want:=2*i+1;if i%2!=0 {want+=100};if v!=want {panic("repeated LIFO snapshot")}}
 trace=nil
 returned(false);returned(true);equal(69,170)
 zero(false);zero(true);equal(7,8)
 variadic(false);variadic(true);equal(7,107)
 before:=evaluations
 if nilCall(true)==nil || evaluations!=before+1 {panic("nil defer did not wait for cleanup")}
 before=evaluations
 if nilArgument(true)==nil || evaluations!=before+2 {panic("nil callee operand evaluation")}
 if repeatedNil()==nil {panic("repeated nil defer")};equal(7,7,7)
 if v:=nilReplacesPanic(true);v==nil || v=="original" {panic("nil defer did not replace body panic")}
 done:=make(chan struct{});go nilDuringExit(true,done);<-done
 if exitMarker!=1 {panic("nil defer recovered sticky Goexit")}
 if argumentPanic(false)!="argument" || argumentPanic(true)!="argument" {panic("argument panic ownership")}
 equal()
 if implicitPanic(false)==nil || implicitPanic(true)==nil {panic("implicit target panic")}
 if bodyPanic(false)!="body" || bodyPanic(true)!="body" {panic("body panic ownership")};equal(7,8)
 for _,which:=range []bool{false,true} {done:=make(chan struct{});go exiting(which,done);<-done};equal(7,8)
 open(zeroA);globalCall();captured();addressed();literal(zeroA);suspended(true);equal(7,7,8,8,7)
 if terminal(true)!="target" {panic("terminal fallback")}
 terminalRecover(true)
 done=make(chan struct{});go terminalExit(true,done);<-done
 close(gcRequest)
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"open", "defer has no static call plan"},
			{"globalCall", "defer has no static call plan"},
			{"captured", "defer has no static call plan"},
			{"addressed", "defer has no static call plan"},
			{"literal", "suspending defer"},
			{"suspended", "suspending defer"},
		},
		[]string{"snapshot", "repeated", "returned", "zero", "variadic", "nilCall", "nilArgument", "repeatedNil", "nilReplacesPanic", "nilDuringExit", "argumentPanic", "implicitPanic", "bodyPanic", "exiting", "terminal", "terminalRecover", "terminalExit"})
}

func TestDynamicDeferImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/dynamicdefer\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
var Trace []int
func First(n int) int {Trace=append(Trace,n);return n}
func Second(n int) int {Trace=append(Trace,n+100);return n}
`,
		"main.go": `package main
import (
 "runtime"
 "example.com/dynamicdefer/leaf"
)
func deferred(which bool) {f:=leaf.First;if which {f=leaf.Second};defer f(7);runtime.Gosched()}
func main() {
 deferred(false);deferred(true)
 if len(leaf.Trace)!=2 || leaf.Trace[0]!=7 || leaf.Trace[1]!=107 {panic("imported deferred target")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/dynamicdefer/...=", nil, []string{"deferred"})
}
