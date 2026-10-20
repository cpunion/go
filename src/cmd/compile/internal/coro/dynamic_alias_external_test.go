// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestDynamicAlias(t *testing.T) {
	const source = `package main
import "runtime"
type callback func(int)int
func first(x int) int {runtime.Gosched();return x+10}
func second(x int) int {runtime.Gosched();return x+20}
func chain(which bool) int {
 a:=first
 if which {a=second}
 b:=a;c:=callback(b);d:=c
 a=nil
 value:=d(3)
 runtime.Gosched()
 return value
}
func swapped(n int) int {
 a,b:=first,second
 total:=0
 for i:=0;i<n;i++ {
  a,b=b,a
  c:=a
  value:=c(i)
  total+=value
 }
 return total
}
func cycle(which bool) int {
 a:=first;b:=a
 if which {b=second}
 a=b;b=a
 value:=b(4)
 return value
}
func self(n int) int {
 a:=first
 total:=0
 for i:=0;i<n;i++ {
  a=a
  b:=a
  value:=b(i)
  total+=value
 }
 return total
}
var arguments int
func argument() int {arguments++;return 7}
func nilAlias(which bool) (result any) {
 defer func(){if value:=recover();value!=nil {result=value}}()
 a:=first
 if which {a=nil}
 b:=a;c:=b
 value:=c(argument())
 return value
}
func failing(x int) int {defer func(){arguments++}();runtime.Gosched();panic(x)}
func mixed(which bool) (result any) {
 defer func(){if value:=recover();value!=nil {result=value}}()
 a:=first
 if which {a=failing}
 b:=a;c:=b
 value:=c(81)
 return value
}
var gcRequest=make(chan bool)
var gcAck=make(chan bool)
func pairA(x *int) (*int,int) {runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func pairB(x *int) (p *int,n int) {defer func(){n++}();runtime.Gosched();gcRequest<-true;<-gcAck;return x,*x+1}
func pair(which bool) int {
 a:=pairA
 if which {a=pairB}
 b:=a;c:=b
 x:=40
 p,n:=c(&x)
 runtime.Gosched()
 if p!=&x || *p!=40 {panic("alias pointer lifetime")}
 return n
}
// Every unknown incoming value poisons downstream copies, including known
// writes later in the body. Copying a function value does not alias its cell.
var global=second
func parameter(a func(int)int) int {b:=a;c:=b;runtime.Gosched();value:=c(1);return value}
func globalCopy() int {a:=global;b:=a;runtime.Gosched();value:=b(2);return value}
func addressed(which bool) int {a:=first;p:=&a;if which {*p=second};b:=a;c:=b;runtime.Gosched();value:=c(3);return value}
func captured(which bool) int {a:=first;change:=func(){a=second};if which {change()};b:=a;c:=b;runtime.Gosched();value:=c(4);return value}
func lateUnknown(input func(int)int) int {a:=first;b:=a;c:=b;value:=c(5);a=input;return value}
func typeCase(input any, replace bool) int {
 switch a:=input.(type) {
 case func(int)int:
  if replace {a=first}
  b:=a;c:=b
  value:=c(6)
  return value
 default:return 0
 }
}
func rangeCopy(values []func(int)int) int {
 total:=0
 for _,a:=range values {
  b:=a;c:=b
  value:=c(7)
  total+=value
 }
 return total
}
func returned(input func(int)int) (func(int)int,bool) {runtime.Gosched();return input,true}
func multiResult(input func(int)int) int {a,_:=returned(input);b:=a;c:=b;value:=c(8);return value}
func missing(x int) int {runtime.Gosched();goto end;end:return x+30}
func missingFactory(which bool) int {a:=first;if which {a=missing};b:=a;c:=b;value:=c(9);return value}
func operation() {a:=runtime.Gosched;b:=a;b()}
func main(){
 if chain(false)!=13 || chain(true)!=23 {panic("alias copy and conversion")}
 if swapped(8)!=148 || cycle(false)!=14 || cycle(true)!=24 || self(4)!=46 {panic("alias fixed point")}
 if nilAlias(false)!=17 || arguments!=1 {panic("alias arguments")}
 if nilAlias(true).(runtime.Error).Error()!="runtime error: invalid memory address or nil pointer dereference" || arguments!=2 {panic("alias nil arguments")}
 if mixed(false)!=91 || mixed(true)!=81 || arguments!=3 {panic("alias mixed factory panic")}
 go func(){for i:=0;i<2;i++ {<-gcRequest;runtime.GC();gcAck<-true}}()
 if pair(false)!=41 || pair(true)!=42 {panic("alias pair results")}
 if parameter(second)!=21 || globalCopy()!=22 || addressed(true)!=23 || captured(true)!=24 || lateUnknown(second)!=15 {panic("alias unknown source")}
 if typeCase(second,false)!=26 || typeCase(second,true)!=16 {panic("alias implicit case")}
 if rangeCopy([]func(int)int{first,second})!=44 || multiResult(second)!=28 || missingFactory(true)!=39 {panic("alias implicit writes or dependency")}
 operation()
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"parameter", "dynamic await site"}, {"globalCopy", "dynamic await site"},
			{"addressed", "dynamic await site"}, {"captured", "dynamic await site"},
			{"lateUnknown", "dynamic await site"}, {"typeCase", "dynamic await site"},
			{"rangeCopy", "dynamic await site"}, {"multiResult", "dynamic await site"},
			{"missingFactory", "unsupported coroutine dependency main.missing"},
		},
		[]string{"chain", "swapped", "cycle", "self", "nilAlias", "mixed", "pair"})
}

func TestDynamicAliasImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/coroalias\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(x int) int {runtime.Gosched();return x+10}
func Second(x int) (result int) {defer func(){result++}();runtime.Gosched();return x+20}
//go:noinline
func Missing(x int) int {runtime.Gosched();goto end;end:return x+30}
`,
		"main.go": `package main
import "example.com/coroalias/leaf"
type callback func(int)int
func imported(which bool) int {
 a:=leaf.First
 if which {a=leaf.Second}
 b:=callback(a);c:=b
 value:=c(2)
 return value
}
func missingFactory(which bool) int {
 a:=leaf.First
 if which {a=leaf.Missing}
 b:=a;c:=b
 value:=c(3)
 return value
}
func main(){
 if imported(false)!=12 || imported(true)!=23 || missingFactory(true)!=33 {panic("imported alias contracts")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/coroalias/...",
		[]switchFallback{{"missingFactory", "unsupported coroutine dependency example.com/coroalias/leaf.Missing"}},
		[]string{"imported"})
}
