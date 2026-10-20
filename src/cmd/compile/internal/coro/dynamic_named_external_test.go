// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestDynamicNamedResult(t *testing.T) {
	const source = `package main
import "runtime"
type callback func(int)int
var selections,arguments,cleanups,inner int
func argument(x int) int {arguments++;return x}
func first(x int) int {runtime.Gosched();return x+10}
func second(x int) (result int) {defer func(){result++}();runtime.Gosched();return x+20}
func choose(which int) (out callback) {
 defer func(){cleanups++}()
 selections++;runtime.Gosched()
 switch which {case 0:out=first;return;case 1:return callback(second);default:return}
}
func consume(which int) int {f:=choose(which);a:=func(int)int(f);g:=a;v:=g(argument(3));runtime.Gosched();return v}
func repeated() int {total:=0;for i:=0;i<6;i++ {f:=choose(i%2);g:=f;v:=g(i);total+=v};return total}
func inside(which bool) (out callback) {out=first;if which {out=second};v:=out(1);inner+=v;return}
func fromInside(which bool) int {f:=inside(which);v:=f(2);return v}
func chooseCopy(which bool) (out callback) {a:=callback(first);out=a;a=out;if which {a=second};out=a;runtime.Gosched();return}
func fromCopy(which bool) int {f:=chooseCopy(which);g:=f;v:=g(3);return v}
func chooseA(n int) (out callback) {runtime.Gosched();if n==0 {out=first;return};out=chooseB(n-1);return}
func chooseB(n int) (out callback) {runtime.Gosched();if n==0 {return callback(second)};return chooseA(n-1)}
func recursive(n int) int {f:=chooseA(n);g:=f;v:=g(2);return v}
func recovered(which bool) (out callback) {defer func(){recover()}();runtime.Gosched();if which {panic("producer")};out=first;return}
func fromRecovered(which bool) (result any) {
 defer func(){if v:=recover();v!=nil {result=v}}()
 f:=callback(second);f=recovered(which);g:=f;v:=g(argument(4));return v
}
func panicArg() int {panic("argument")}
func nilArgument() (result any) {defer func(){if v:=recover();v!=nil {result=v}}();f:=choose(2);g:=f;v:=g(panicArg());return v}
var gcRequest=make(chan bool)
var gcAck=make(chan bool)
func pairA(x *int) (*int,int) {runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func pairB(x *int) (p *int,n int) {defer func(){n++}();runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func choosePair(which bool) (out func(*int)(*int,int)) {out=pairA;runtime.Gosched();if which {out=pairB};return}
func consumePair(which bool) int {f:=choosePair(which);g:=f;x:=40;p,n:=g(&x);runtime.Gosched();if p!=&x || *p!=40 {panic("named callback GC")};return n}
func failing(x int) int {runtime.Gosched();panic(x)}
func choosePanic(which bool) (out callback) {out=first;runtime.Gosched();if which {out=failing};return}
func childPanic(which bool) (result any) {defer func(){if v:=recover();v!=nil {result=v}}();f:=choosePanic(which);v:=f(71);return v}
func exiting(done chan int) {defer func(){cleanups++}();runtime.Gosched();done<-1;runtime.Goexit()}
func finishing(done chan int) {runtime.Gosched();done<-2}
func chooseExit(which bool) (out func(chan int)) {out=finishing;runtime.Gosched();if which {out=exiting};return}
func childExit(which bool,done chan int) {defer func(){close(done)}();f:=chooseExit(which);f(done);done<-4}
var global=callback(second)
//go:noinline
func chooseParameter(input callback) (out callback) {out=first;runtime.Gosched();out=input;return}
//go:noinline
func chooseGlobal() (out callback) {runtime.Gosched();out=global;return}
//go:noinline
func chooseAddress(input callback) (out callback) {out=first;p:=&out;*p=input;runtime.Gosched();return}
//go:noinline
func chooseCapture(input callback) (out callback) {out=first;change:=func(){out=input};change();runtime.Gosched();return}
//go:noinline
func chooseDeferred(input callback) (out callback) {defer func(){out=input}();runtime.Gosched();return callback(first)}
//go:noinline
func chooseAlias(input callback) (out callback) {a:=callback(first);change:=func(){a=input};change();out=a;runtime.Gosched();return}
//go:noinline
func chooseRange() (out callback) {out=first;for _,out=range []callback{second} {};runtime.Gosched();return}
//go:noinline
func chooseCase(input any,replace bool) (out callback) {out=first;switch f:=input.(type) {case callback:if replace {f=first};out=f};runtime.Gosched();return}
func parameter() int {f:=chooseParameter(second);g:=f;v:=g(1);return v}
func globalResult() int {f:=chooseGlobal();v:=f(2);return v}
func addressed() int {f:=chooseAddress(second);v:=f(3);return v}
func captured() int {f:=chooseCapture(second);v:=f(4);return v}
func deferred() int {f:=chooseDeferred(second);v:=f(5);return v}
func aliasCapture() int {f:=chooseAlias(second);v:=f(6);return v}
func rangeResult() int {f:=chooseRange();v:=f(7);return v}
func caseResult(replace bool) int {f:=chooseCase(callback(second),replace);v:=f(8);return v}
func missing(x int) int {runtime.Gosched();goto end;end:return x+30}
func chooseMissing() (out callback) {runtime.Gosched();out=missing;return}
func missingFactory() int {f:=chooseMissing();v:=f(9);return v}
func main(){
 if consume(0)!=13 || consume(1)!=24 || selections!=2 || arguments!=2 || cleanups!=2 {panic("named dispatch or once-only evaluation")}
 if repeated()!=108 || fromInside(false)!=12 || fromInside(true)!=23 || inner!=33 || fromCopy(false)!=13 || fromCopy(true)!=24 {panic("named slot copies or direct invocation")}
 if recursive(200)!=12 || recursive(201)!=23 {panic("named recursive result graph")}
 if fromRecovered(false)!=14 {panic("named recovered normal")};before:=arguments
 if fromRecovered(true).(runtime.Error).Error()!="runtime error: invalid memory address or nil pointer dereference" || arguments!=before+1 {panic("named recovered nil result or argument order")}
 if nilArgument()!="argument" {panic("named nil argument panic")}
 go func(){for i:=0;i<2;i++ {<-gcRequest;runtime.GC();gcAck<-true}}()
 if consumePair(false)!=41 || consumePair(true)!=42 {panic("named pointer and multiple callback results")}
 if childPanic(false)!=81 || childPanic(true)!=71 {panic("named child panic")}
 done:=make(chan int,8);go childExit(true,done);if <-done!=1 {panic("named Goexit")};if _,ok:=<-done;ok {panic("named Goexit cleanup")}
 done=make(chan int,8);go childExit(false,done);if <-done!=2 || <-done!=4 {panic("named normal child")};if _,ok:=<-done;ok {panic("named normal cleanup")}
 if parameter()!=22 || globalResult()!=23 || addressed()!=24 || captured()!=25 || deferred()!=26 || aliasCapture()!=27 || rangeResult()!=28 || caseResult(false)!=29 || caseResult(true)!=18 || missingFactory()!=39 {panic("named unknown output or deferred overwrite")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"parameter", "dynamic await site"}, {"globalResult", "dynamic await site"},
			{"addressed", "dynamic await site"}, {"captured", "dynamic await site"},
			{"deferred", "dynamic await site"}, {"aliasCapture", "dynamic await site"},
			{"rangeResult", "dynamic await site"}, {"caseResult", "dynamic await site"},
			{"missingFactory", "unsupported coroutine dependency main.missing"},
		},
		[]string{"consume", "repeated", "inside", "fromInside", "chooseCopy", "fromCopy", "recursive", "chooseA", "chooseB", "choose", "recovered", "fromRecovered", "nilArgument", "choosePair", "consumePair", "childPanic", "childExit"})
}

func TestDynamicNamedResultImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/coronamed\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(x int) int {runtime.Gosched();return x+10}
func Second(x int) (result int) {defer func(){result++}();runtime.Gosched();return x+20}
//go:noinline
func Choose(which bool) (out func(int)int) {out=First;runtime.Gosched();if which {out=Second};return}
//go:noinline
func Missing(x int) int {runtime.Gosched();goto end;end:return x+30}
`,
		"main.go": `package main
import ("runtime";"example.com/coronamed/leaf")
type callback func(int)int
func chooseLocal(which bool) (out callback) {out=callback(leaf.First);runtime.Gosched();if which {return callback(leaf.Second)};return}
func importedTarget(which bool) int {f:=chooseLocal(which);g:=f;v:=g(2);return v}
func importedProducer(which bool) int {f:=leaf.Choose(which);v:=f(3);return v}
func chooseMissing() (out callback) {runtime.Gosched();out=leaf.Missing;return}
func missingFactory() int {f:=chooseMissing();v:=f(4);return v}
func main(){if importedTarget(false)!=12 || importedTarget(true)!=23 || importedProducer(false)!=13 || importedProducer(true)!=24 || missingFactory()!=34 {panic("named imported contracts")};println("switch-return-ok")}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/coronamed/...=",
		[]switchFallback{{"importedProducer", "dynamic await site"}, {"missingFactory", "unsupported coroutine dependency example.com/coronamed/leaf.Missing"}},
		[]string{"chooseLocal", "importedTarget"})
}
