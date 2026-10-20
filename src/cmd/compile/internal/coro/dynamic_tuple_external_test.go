// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestDynamicTupleResult(t *testing.T) {
	const source = `package main
import "runtime"
type callback func(int)int
var selections,arguments,cleanups int
func argument(x int) int {arguments++;return x}
func first(x int) int {runtime.Gosched();return x+10}
func second(x int) (result int) {defer func(){result++}();runtime.Gosched();return x+20}
func choose(which int) (func(int)int,int,func(int)int) {selections++;runtime.Gosched();switch which {case 0:return first,40,second;case 1:return second,50,first;default:return nil,60,first}}
func forward(which int) (func(int)int,int,func(int)int) {return choose(which)}
func converted(which int) (a callback,n int,b callback) {defer func(){n++;cleanups++}();return choose(which)}
func consume(which int) int {a,n,b:=forward(which);x:=a(argument(1));y:=b(argument(2));runtime.Gosched();return x*100+y+n}
func consumeConverted(which int) int {a,n,b:=converted(which);x:=a(3);y:=b(4);return x*100+y+n}
func repeated() int {total:=0;for i:=0;i<6;i++ {a,n,b:=choose(i%2);a,b=b,a;x:=a(i);y:=b(i);total+=x+y+n};return total}
func chooseSmall(which bool) (func(int)int,bool) {runtime.Gosched();if which {return second,true};return first,false}
func small(which bool) int {f,ok:=chooseSmall(which);v:=f(5);if ok {v++};return v}
func named(which bool) (a callback,n int,b callback) {a=first;n=3;b=second;runtime.Gosched();if which {return callback(second),4,callback(first)};return}
func consumeNamed(which bool) int {a,n,b:=named(which);x:=a(6);y:=b(7);return x*100+y+n}
func cycleA(n int) (func(int)int,int,func(int)int) {runtime.Gosched();if n==0 {return first,7,second};return cycleB(n-1)}
func cycleB(n int) (func(int)int,int,func(int)int) {runtime.Gosched();if n==0 {return second,8,first};return cycleA(n-1)}
func recursive(n int) int {a,tag,b:=cycleA(n);x:=a(2);y:=b(3);return x*100+y+tag}
func recovered(which bool) (func(int)int,int,func(int)int) {defer func(){recover()}();runtime.Gosched();if which {panic("producer")};return first,9,second}
func fromRecovered(which bool) (result any) {
 defer func(){if v:=recover();v!=nil {result=v}}()
 a,b,n:=callback(second),callback(first),99
 a,n,b=recovered(which)
 if which && (n!=0 || b!=nil) {panic("tuple recovered result overwrite")}
 x:=a(argument(4));y:=b(5);return x*100+y+n
}
func panicArg() int {panic("argument")}
func nilArgument() (result any) {defer func(){if v:=recover();v!=nil {result=v}}();f,_,_:=choose(2);v:=f(panicArg());return v}
var gcRequest=make(chan bool)
var gcAck=make(chan bool)
func pairA(x *int) (*int,int) {runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func pairB(x *int) (p *int,n int) {defer func(){n++}();runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func choosePair(which bool) (*int,int,func(*int)(*int,int)) {x:=40;runtime.Gosched();if which {return &x,2,pairB};return &x,1,pairA}
func consumePair(which bool) int {p,tag,f:=choosePair(which);q,n:=f(p);runtime.Gosched();if q!=p || *q!=40 {panic("tuple callback GC")};return n+tag}
func failing(x int) int {runtime.Gosched();panic(x)}
func choosePanic(which bool) (callback,bool) {runtime.Gosched();if which {return callback(failing),true};return callback(first),false}
func childPanic(which bool) (result any) {defer func(){if v:=recover();v!=nil {result=v}}();f,ok:=choosePanic(which);if ok!=which {panic("tuple panic flag")};v:=f(71);return v}
func exiting(done chan int) {defer func(){cleanups++}();runtime.Gosched();done<-1;runtime.Goexit()}
func finishing(done chan int) {runtime.Gosched();done<-2}
func chooseExit(which bool) (int,func(chan int),bool) {runtime.Gosched();if which {return 1,exiting,true};return 2,finishing,false}
func childExit(which bool,done chan int) {defer func(){close(done)}();tag,f,ok:=chooseExit(which);if ok!=which {panic("tuple exit flag")};f(done);done<-tag+10}
// Unknown function-valued slots are independent from other returned slots.
//go:noinline
func split(input callback) (callback,int,callback) {runtime.Gosched();return callback(first),6,input}
func useFirst(input callback) int {f,tag,_:=split(input);v:=f(3);return v+tag}
func unknownSecond(input callback) int {_,tag,g:=split(input);v:=g(3);return v+tag}
//go:noinline
func deferredSplit(input callback) (a callback,n int,b callback) {defer func(){b=input}();runtime.Gosched();return callback(first),7,callback(first)}
func useDeferredFirst(input callback) int {a,n,_:=deferredSplit(input);v:=a(4);return v+n}
func deferredSecond(input callback) int {_,n,b:=deferredSplit(input);v:=b(4);return v+n}
//go:noinline
func addressSplit(input callback) (a callback,n int,b callback) {a=first;n=8;b=second;p:=&b;*p=input;runtime.Gosched();return}
func useAddressFirst(input callback) int {a,n,_:=addressSplit(input);v:=a(5);return v+n}
func addressSecond(input callback) int {_,n,b:=addressSplit(input);v:=b(5);return v+n}
//go:noinline
func caseSplit(input any,replace bool) (callback,int,callback) {runtime.Gosched();switch f:=input.(type) {case callback:if replace {f=first};return callback(first),9,f;default:return callback(first),9,nil}}
func useCaseFirst() int {a,n,_:=caseSplit(callback(second),false);v:=a(6);return v+n}
func caseSecond(replace bool) int {_,n,b:=caseSplit(callback(second),replace);v:=b(6);return v+n}
func missing(x int) int {runtime.Gosched();goto end;end:return x+30}
func missingSplit() (callback,int,callback) {runtime.Gosched();return callback(first),10,callback(missing)}
func useMissingFirst() int {a,n,_:=missingSplit();v:=a(7);return v+n}
func missingSecond() int {_,n,b:=missingSplit();v:=b(7);return v+n}
func withScalar(input int) (callback,int,callback) {runtime.Gosched();return callback(first),input,callback(second)}
func scalarResult(input int) int {a,n,b:=withScalar(input);x:=a(3);y:=b(3);return x*100+y+n}
func reverse(which int) (func(int)int,int,func(int)int) {runtime.Gosched();return second,11,first}
func dynamicProducer(which bool) int {p:=choose;if which {p=reverse};f,_,_:=p(0);v:=f(8);return v}
func main(){
 if consume(0)!=1163 || consume(1)!=2262 || selections!=2 || arguments!=4 {panic("tuple slots or once-only evaluation")}
 if consumeConverted(0)!=1366 || consumeConverted(1)!=2465 || cleanups!=2 {panic("tuple conversion or deferred scalar result")}
 if repeated()!=486 || small(false)!=15 || small(true)!=27 || consumeNamed(false)!=1631 || consumeNamed(true)!=2721 {panic("tuple assignments or named results")}
 if recursive(200)!=1231 || recursive(201)!=2321 {panic("tuple recursive return forwarding")}
 if fromRecovered(false)!=1435 {panic("tuple recovered ordinary result")};before:=arguments
 if fromRecovered(true).(runtime.Error).Error()!="runtime error: invalid memory address or nil pointer dereference" || arguments!=before+1 {panic("tuple recovered zero results or nil argument order")}
 if nilArgument()!="argument" {panic("tuple nil argument panic")}
 go func(){for i:=0;i<2;i++ {<-gcRequest;runtime.GC();gcAck<-true}}()
 if consumePair(false)!=42 || consumePair(true)!=44 {panic("tuple pointer or callback result lifetime")}
 if childPanic(false)!=81 || childPanic(true)!=71 {panic("tuple child panic")}
 done:=make(chan int,8);go childExit(true,done);if <-done!=1 {panic("tuple Goexit")};if _,ok:=<-done;ok {panic("tuple Goexit cleanup")}
 done=make(chan int,8);go childExit(false,done);if <-done!=2 || <-done!=12 {panic("tuple normal child")};if _,ok:=<-done;ok {panic("tuple normal cleanup")}
 if useFirst(second)!=19 || unknownSecond(second)!=30 || useDeferredFirst(second)!=21 || deferredSecond(second)!=32 {panic("tuple independent unknown or deferred slots")}
 if useAddressFirst(second)!=23 || addressSecond(second)!=34 || useCaseFirst()!=25 || caseSecond(false)!=36 || caseSecond(true)!=25 {panic("tuple addressed or implicit slot")}
 if useMissingFirst()!=27 || missingSecond()!=47 || scalarResult(30)!=1354 || dynamicProducer(false)!=18 || dynamicProducer(true)!=29 {panic("tuple factory or dynamic producer boundary")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"unknownSecond", "dynamic await site"}, {"deferredSecond", "dynamic await site"},
			{"addressSecond", "dynamic await site"}, {"caseSecond", "dynamic await site"},
			{"missingSecond", "unsupported coroutine dependency main.missing"}, {"dynamicProducer", "dynamic await site"},
		},
		[]string{"choose", "forward", "converted", "consume", "consumeConverted", "repeated", "small", "named", "consumeNamed", "cycleA", "cycleB", "recursive", "recovered", "fromRecovered", "nilArgument", "choosePair", "consumePair", "childPanic", "childExit", "useFirst", "useDeferredFirst", "useAddressFirst", "useCaseFirst", "useMissingFirst", "scalarResult"})
}

func TestDynamicTupleResultImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/corotuple\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(x int) int {runtime.Gosched();return x+10}
func Second(x int) (n int) {defer func(){n++}();runtime.Gosched();return x+20}
//go:noinline
func Choose(which bool) (func(int)int,int,func(int)int) {runtime.Gosched();if which {return Second,4,First};return First,3,Second}
//go:noinline
func Missing(x int) int {runtime.Gosched();goto end;end:return x+30}
`,
		"main.go": `package main
import ("runtime";"example.com/corotuple/leaf")
type callback func(int)int
func chooseLocal(which bool) (callback,int,callback) {runtime.Gosched();if which {return callback(leaf.Second),4,callback(leaf.First)};return callback(leaf.First),3,callback(leaf.Second)}
func forward(which bool) (callback,int,callback) {return chooseLocal(which)}
func importedTarget(which bool) int {a,n,b:=forward(which);x:=a(2);y:=b(3);return x*100+y+n}
func importedProducer(which bool) int {a,n,b:=leaf.Choose(which);x:=a(2);y:=b(3);return x*100+y+n}
func missingSplit() (callback,int,callback) {runtime.Gosched();return callback(leaf.First),5,callback(leaf.Missing)}
func unusedMissing() int {a,n,_:=missingSplit();v:=a(4);return v+n}
func usedMissing() int {_,n,b:=missingSplit();v:=b(4);return v+n}
func main(){if importedTarget(false)!=1227 || importedTarget(true)!=2317 || importedProducer(false)!=1227 || importedProducer(true)!=2317 || unusedMissing()!=19 || usedMissing()!=39 {panic("tuple imported contracts")};println("switch-return-ok")}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/corotuple/...=",
		[]switchFallback{{"importedProducer", "dynamic await site"}, {"usedMissing", "unsupported coroutine dependency example.com/corotuple/leaf.Missing"}},
		[]string{"chooseLocal", "forward", "importedTarget", "unusedMissing"})
}
