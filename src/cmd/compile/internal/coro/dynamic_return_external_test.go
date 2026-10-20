// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestDynamicReturn(t *testing.T) {
	const source = `package main
import "runtime"
type callback func(int)int
var selections,arguments,cleanups int
func argument(x int) int {arguments++;return x}
func first(x int) int {runtime.Gosched();return x+10}
func second(x int) (result int) {defer func(){result++}();runtime.Gosched();return x+20}
func choose(which int) func(int)int {
 selections++
 runtime.Gosched()
 switch which {case 0:return first;case 1:return second;default:return nil}
}
func consume(which int) int {
 f:=choose(which);a:=callback(f);b:=a
 value:=b(argument(3))
 runtime.Gosched()
 return value
}
func repeated() int {
 total:=0
 for i:=0;i<6;i++ {
  f:=choose(i%2);g:=f
  value:=g(i)
  total+=value
 }
 return total
}
func chooseA(n int) func(int)int {runtime.Gosched();if n==0 {return first};return chooseB(n-1)}
func chooseB(n int) func(int)int {runtime.Gosched();if n==0 {return second};return chooseA(n-1)}
func recursive(n int) int {f:=chooseA(n);g:=f;value:=g(2);return value}
func chooseDefer(which bool) func(int)int {defer func(){cleanups++}();runtime.Gosched();if which {return second};return first}
func fromDefer(which bool) int {f:=chooseDefer(which);value:=f(4);return value}
func nilReturned(which int) (result any) {
 defer func(){if value:=recover();value!=nil {result=value}}()
 f:=choose(which);g:=f
 value:=g(argument(9))
 return value
}
func panicArgument() int {panic("argument")}
func nilArgument() (result any) {
 defer func(){if value:=recover();value!=nil {result=value}}()
 f:=choose(2);g:=f
 value:=g(panicArgument())
 return value
}
var gcRequest=make(chan bool)
var gcAck=make(chan bool)
func pairA(x *int) (*int,int) {runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func pairB(x *int) (p *int,n int) {defer func(){n++}();runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func choosePair(which bool) func(*int)(*int,int) {runtime.Gosched();if which {return pairB};return pairA}
func consumePair(which bool) int {
 f:=choosePair(which);g:=f
 x:=40
 p,n:=g(&x)
 runtime.Gosched()
 if p!=&x || *p!=40 {panic("returned callback GC lifetime")}
 return n
}
func failing(x int) int {runtime.Gosched();panic(x)}
func choosePanic(which bool) func(int)int {runtime.Gosched();if which {return failing};return first}
func childPanic(which bool) (result any) {
 defer func(){if value:=recover();value!=nil {result=value}}()
 f:=choosePanic(which);g:=f
 value:=g(71)
 return value
}
func exiting(done chan int) {defer func(){cleanups++}();runtime.Gosched();done<-1;runtime.Goexit()}
func finishing(done chan int) {runtime.Gosched();done<-2}
func chooseExit(which bool) func(chan int) {runtime.Gosched();if which {return exiting};return finishing}
func childExit(which bool,done chan int) {
 defer func(){cleanups++;close(done)}()
 f:=chooseExit(which);g:=f
 g(done)
 done<-4
}
// A return proof must reject all incoming unknown values, including values
// written after the call or supplied by implicit bindings and deferred results.
var global=second
//go:noinline
func chooseParameter(input func(int)int) func(int)int {runtime.Gosched();return input}
//go:noinline
func chooseGlobal() func(int)int {runtime.Gosched();return global}
//go:noinline
func chooseAddress(which bool) func(int)int {a:=first;p:=&a;if which {*p=second};runtime.Gosched();return a}
//go:noinline
func chooseCapture(which bool) func(int)int {a:=first;change:=func(){a=second};if which {change()};runtime.Gosched();return a}
//go:noinline
func chooseClosure(x int) func(int)int {runtime.Gosched();return func(y int) int {runtime.Gosched();return x+y}}
//go:noinline
func chooseCase(input any,replace bool) func(int)int {switch a:=input.(type) {case func(int)int:if replace {a=first};return a;default:return nil}}
//go:noinline
func chooseNamed(input func(int)int) (result func(int)int) {defer func(){result=input}();runtime.Gosched();return first}
//go:noinline
func chooseMulti(which bool) (func(int)int,bool) {runtime.Gosched();if which {return second,true};return first,false}
func parameter(input func(int)int) int {f:=chooseParameter(input);g:=f;value:=g(1);return value}
func globalResult() int {f:=chooseGlobal();value:=f(2);return value}
func addressed() int {f:=chooseAddress(true);value:=f(3);return value}
func captured() int {f:=chooseCapture(true);value:=f(4);return value}
func closureResult() int {f:=chooseClosure(30);value:=f(5);return value}
func caseResult(replace bool) int {f:=chooseCase(second,replace);g:=f;value:=g(6);return value}
func namedResult() int {f:=chooseNamed(second);g:=f;value:=g(7);return value}
func multipleResult() int {f,_:=chooseMulti(true);value:=f(8);return value}
func lateUnknown(input func(int)int) int {f:=choose(0);g:=f;value:=g(9);f=input;value+=f(1);return value}
func missing(x int) int {runtime.Gosched();goto end;end:return x+30}
func chooseMissing(which bool) func(int)int {runtime.Gosched();if which {return missing};return first}
func missingFactory() int {f:=chooseMissing(true);value:=f(10);return value}
func main(){
 if consume(0)!=13 || consume(1)!=24 || selections!=2 || arguments!=2 {panic("returned dispatch or evaluation")}
 if repeated()!=108 || recursive(200)!=12 || recursive(201)!=23 {panic("returned chain or recursive fixed point")}
 if fromDefer(false)!=14 || fromDefer(true)!=25 || cleanups!=2 {panic("producer factory or defer")}
 before:=arguments
 if nilReturned(2).(runtime.Error).Error()!="runtime error: invalid memory address or nil pointer dereference" || arguments!=before+1 {panic("returned nil order")}
 if nilArgument()!="argument" {panic("returned nil argument panic")}
 go func(){for i:=0;i<2;i++ {<-gcRequest;runtime.GC();gcAck<-true}}()
 if consumePair(false)!=41 || consumePair(true)!=42 {panic("returned pair callback")}
 if childPanic(false)!=81 || childPanic(true)!=71 {panic("returned child panic")}
 done:=make(chan int,8);go childExit(true,done)
 if <-done!=1 {panic("returned Goexit child")};if _,ok:=<-done;ok {panic("returned Goexit cleanup")}
 done=make(chan int,8);go childExit(false,done)
 if <-done!=2 || <-done!=4 {panic("returned normal child")};if _,ok:=<-done;ok || cleanups!=5 {panic("returned parent cleanup")}
 if parameter(second)!=22 || globalResult()!=23 || addressed()!=24 || captured()!=25 || closureResult()!=35 {panic("returned unknown source")}
 if caseResult(false)!=27 || caseResult(true)!=16 || namedResult()!=28 || multipleResult()!=29 || lateUnknown(second)!=41 || missingFactory()!=40 {panic("returned implicit or deferred overwrite")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"parameter", "dynamic await site"}, {"globalResult", "dynamic await site"},
			{"addressed", "dynamic await site"}, {"captured", "dynamic await site"},
			{"closureResult", "dynamic await site"}, {"caseResult", "dynamic await site"},
			{"namedResult", "dynamic await site"},
			{"lateUnknown", "dynamic await site"}, {"missingFactory", "unsupported coroutine dependency main.missing"},
		},
		[]string{"consume", "repeated", "recursive", "fromDefer", "nilReturned", "nilArgument", "consumePair", "childPanic", "childExit", "choose", "chooseA", "chooseB", "chooseDefer", "choosePair", "multipleResult", "chooseMulti"})
}

func TestDynamicReturnImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/cororeturn\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(x int) int {runtime.Gosched();return x+10}
func Second(x int) (result int) {defer func(){result++}();runtime.Gosched();return x+20}
//go:noinline
func Missing(x int) int {runtime.Gosched();goto end;end:return x+30}
//go:noinline
func Choose(which bool) func(int)int {runtime.Gosched();if which {return Second};return First}
`,
		"main.go": `package main
import (
 "runtime"
 "example.com/cororeturn/leaf"
)
type callback func(int)int
func chooseLocal(which bool) callback {runtime.Gosched();if which {return callback(leaf.Second)};return callback(leaf.First)}
func importedTarget(which bool) int {f:=chooseLocal(which);g:=f;value:=g(2);return value}
func importedProducer(which bool) int {f:=leaf.Choose(which);g:=f;value:=g(3);return value}
func chooseMissing(which bool) func(int)int {runtime.Gosched();if which {return leaf.Missing};return leaf.First}
func missingFactory(which bool) int {f:=chooseMissing(which);g:=f;value:=g(4);return value}
func main(){
 if importedTarget(false)!=12 || importedTarget(true)!=23 || importedProducer(false)!=13 || importedProducer(true)!=24 || missingFactory(true)!=34 {panic("returned imported contract")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/cororeturn/...=",
		[]switchFallback{{"importedProducer", "dynamic await site"}, {"missingFactory", "unsupported coroutine dependency example.com/cororeturn/leaf.Missing"}},
		[]string{"importedTarget", "chooseLocal"})
}
