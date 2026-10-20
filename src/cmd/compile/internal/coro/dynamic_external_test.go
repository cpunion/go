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
var gcRequest=make(chan bool)
var gcAck=make(chan bool)
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
  value := f(i)
  total += value
 }
 return total
}
func pairA(x *int) (*int,int) { runtime.Gosched(); gcRequest<-true; <-gcAck; return x,*x+1 }
func pairB(x *int) (*int,int) { defer func(){trace+="b;"}(); runtime.Gosched(); gcRequest<-true; <-gcAck; return x,*x+2 }
func pair(which bool) int {
 f:=pairA
 if which {f=pairB}
 x:=41
 p,n:=f(&x)
 runtime.Gosched()
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
func nilSingle(which bool) (result any) {
 defer func(){if value:=recover();value!=nil {result=value}}()
 f:=first
 if which {f=nil}
 value:=f(argument(1))
 return value
}
func voidA() {runtime.Gosched();trace+="a;"}
func voidB() {defer func(){trace+="v;"}();runtime.Gosched()}
func discarded(which bool) {
 f:=first
 if which {f=second}
 f(argument(8))
 v:=voidA
 if which {v=voidB}
 v()
}
func failing(x int) int {runtime.Gosched(); panic(x)}
func childPanic(which bool) (result any) {
 defer func(){if value:=recover();value!=nil {result=value}}()
 f:=first
 if which {f=failing}
 value:=f(71)
 return value
}
func exiting(done chan int) {defer func(){trace+="exit;"}(); runtime.Gosched(); done<-1; runtime.Goexit()}
func finishing(done chan int) {runtime.Gosched(); done<-2}
func childExit(which bool, done chan int) {
 defer func(){trace+="parent;";close(done)}()
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
func caseBinding(input any, replace bool) int {
 switch f:=input.(type) {
 case func(int)int:
  if replace {f=first}
  value:=f(6)
  return value
 default:return 0
 }
}
func closedInCase(input any, which bool) int {
 switch x:=input.(type) {
 case int:
  f:=first
  if which {f=second}
  value:=f(x)
  return value
 default:return 0
 }
}
func main(){
 if choose(0)!=13 || choose(1)!=23 || arguments!=2 {panic("selection or argument evaluation")}
 if repeated()!=105 {panic("repeated dispatch")}
 go func(){for i:=0;i<2;i++ {<-gcRequest;runtime.GC();gcAck<-true}}()
 if pair(false)!=42 || pair(true)!=43 || trace!="b;" {panic("mixed factory results")}
 if variadic(false)!=6 || variadic(true)!=11 {panic("variadic values")}
 before:=arguments
 nilValue:=nilCall(0)
 if nilValue.(runtime.Error).Error()!="runtime error: invalid memory address or nil pointer dereference" || arguments!=before+1 {panic("nil evaluation order")}
 before=arguments
 if nilSingle(true).(runtime.Error).Error()!="runtime error: invalid memory address or nil pointer dereference" || arguments!=before+1 {panic("single nil target")}
 discarded(false);discarded(true)
 if trace!="b;a;v;" {panic("discarded calls")}
 if childPanic(false)!=81 || childPanic(true)!=71 {panic("child panic")}
 done:=make(chan int,8)
 go childExit(true,done)
 if <-done!=1 {panic("Goexit child")}
 if _,ok:=<-done;ok || trace!="b;a;v;exit;parent;" {panic("Goexit cleanup")}
 done=make(chan int,8)
 go childExit(false,done)
 if <-done!=2 || <-done!=4 {panic("normal child")}
 if _,ok:=<-done;ok || trace!="b;a;v;exit;parent;parent;" {panic("normal cleanup")}
 if parameter(second)!=21 || globalCall(true)!=22 || addressed(true)!=23 || captured(true)!=24 || missingFactory(true)!=35 {panic("fallback result")}
 operation(true)
 if caseBinding(second,false)!=26 || caseBinding(second,true)!=16 {panic("implicit type-case target")}
 if closedInCase(2,false)!=12 || closedInCase(2,true)!=22 {panic("closed target inside type case")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"parameter", "dynamic await site"}, {"globalCall", "dynamic await site"},
			{"addressed", "dynamic await site"}, {"captured", "dynamic await site"},
			{"missingFactory", "unsupported coroutine dependency main.missing"},
			{"caseBinding", "dynamic await site"},
		},
		[]string{"choose", "repeated", "pair", "variadic", "nilCall", "nilSingle", "discarded", "childPanic", "childExit", "closedInCase"})
}

func TestDynamicCallImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/corodynamic\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(x int) int {runtime.Gosched();return x+10}
func Second(x int) (result int) {defer func(){result++}();runtime.Gosched();return x+20}
//go:noinline
func Missing(x int) int {runtime.Gosched();goto end;end:return x+30}
`,
		"main.go": `package main
import "example.com/corodynamic/leaf"
type callback func(int)int
func imported(which bool) int {
 f:=callback(leaf.First)
 if which {f=callback(leaf.Second)}
 value:=f(3)
 return value
}
func missingImported(which bool) int {
 f:=leaf.First
 if which {f=leaf.Missing}
 value:=f(4)
 return value
}
func main(){
 if imported(false)!=13 || imported(true)!=24 || missingImported(true)!=34 {panic("imported dispatch")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/corodynamic/...=",
		[]switchFallback{{"missingImported", "unsupported coroutine dependency example.com/corodynamic/leaf.Missing"}},
		[]string{"imported"})
}
