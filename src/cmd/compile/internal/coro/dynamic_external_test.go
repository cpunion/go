// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestDynamicCall(t *testing.T) {
	const source = `package main
import "runtime"

var arguments int
var trace string
func argument(x int) int { arguments++; return x }
func first(x int) int { runtime.Gosched(); return x+10 }
func second(x int) int { runtime.Gosched(); return x+20 }
func choose(which int) int {
 f := first
 if which==1 { f=second }
 result := f(argument(3))
 runtime.Gosched()
 return result
}
func repeated() int {
 f := first
 total := 0
 for i:=0;i<6;i++ {
  if i%2==0 {f=first} else {f=second}
  total += f(i)
 }
 return total
}
func pairA(x *int) (*int,int) { runtime.Gosched(); runtime.GC(); return x,*x+1 }
func pairB(x *int) (*int,int) { defer func(){trace+="b;"}(); runtime.Gosched(); runtime.GC(); return x,*x+2 }
func pair(which bool) int {
 f:=pairA
 if which {f=pairB}
 x:=41
 p,n:=f(&x)
 runtime.GC()
 if p!=&x || *p!=41 {panic("pointer result")}
 return n
}
func variadicA(x int, values ...int) int {runtime.Gosched(); return x+values[0]+values[1]}
func variadicB(x int, values ...int) int {runtime.Gosched(); return x+2*(values[0]+values[1])}
func variadic(which bool) int {
 f:=variadicA
 if which {f=variadicB}
 values:=[]int{2,3}
 result:=f(1,values...)
 return result
}
func nilCall(which int) (result any) {
 defer func(){
  if value:=recover(); value!=nil {result=value}
 }()
 f:=first
 if which==0 {f=nil} else {f=second}
 resultValue:=f(argument(9))
 return resultValue
}
func failing(x int) int {runtime.Gosched(); panic(x)}
func childPanic(which bool) (result any) {
 defer func(){result=recover()}()
 f:=first
 if which {f=failing}
 value:=f(71)
 return value
}
func exiting(done chan int) {defer func(){done<-1}(); runtime.Gosched(); runtime.Goexit()}
func finishing(done chan int) {runtime.Gosched(); done<-2}
func childExit(which bool, done chan int) {
 defer func(){done<-3}()
 f:=finishing
 if which {f=exiting}
 f(done)
 done<-4
}
// Unknown targets and storage that another operation can change retain ordinary Go.
var global=first
func parameter(f func(int)int) int {runtime.Gosched(); return f(1)}
func globalCall(which bool) int {runtime.Gosched(); if which {global=second}; return global(2)}
func addressed(which bool) int {f:=first;p:=&f;if which {*p=second};runtime.Gosched();return f(3)}
func captured(which bool) int {f:=first;change:=func(){f=second};if which {change()};runtime.Gosched();return f(4)}
func missing(x int) int {runtime.Gosched(); goto end; end: return x+30}
func missingFactory(which bool) int {f:=first;if which {f=missing};value:=f(5);return value}
func operation(which bool) {f:=runtime.Gosched;if which {f=runtime.Gosched};f()}
func main(){
 if choose(0)!=13 || choose(1)!=23 || arguments!=2 {panic("selection or argument evaluation")}
 if repeated()!=105 {panic("repeated dispatch")}
 if pair(false)!=42 || pair(true)!=43 || trace!="b;" {panic("mixed factory results")}
 if variadic(false)!=6 || variadic(true)!=11 {panic("variadic values")}
 before:=arguments
 nilValue:=nilCall(0)
 if nilValue.(runtime.Error).Error()!="invalid memory address or nil pointer dereference" || arguments!=before+1 {panic("nil evaluation order")}
 if childPanic(false)!=81 || childPanic(true)!=71 {panic("child panic")}
 done:=make(chan int,8)
 go childExit(true,done)
 if <-done!=1 || <-done!=3 {panic("Goexit cleanup")}
 go childExit(false,done)
 if <-done!=2 || <-done!=4 || <-done!=3 {panic("normal cleanup")}
 if parameter(second)!=21 || globalCall(true)!=22 || addressed(true)!=23 || captured(true)!=24 || missingFactory(true)!=35 {panic("fallback result")}
 operation(true)
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"parameter", "dynamic await site"}, {"globalCall", "dynamic await site"},
			{"addressed", "dynamic await site"}, {"captured", "dynamic await site"},
			{"missingFactory", "unsupported coroutine dependency main.missing"},
		},
		[]string{"choose", "repeated", "pair", "variadic", "nilCall", "childPanic", "childExit"})
}
