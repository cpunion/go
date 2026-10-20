// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestTupleCallInitializers(t *testing.T) {
	const source = `package main
import "runtime"
var trace string
func produce() (int,int) { trace+="p";runtime.Gosched();return 2,3 }
func consume(a,b int) int { trace+="c";runtime.Gosched();return a*10+b }
func alternate(a,b int) int { trace+="a";runtime.Gosched();return a*100+b }
func plain(a,b int) int { trace+="l";return a+b }
func predicate(a,b int) bool { trace+="b";runtime.Gosched();return a+b==5 }
func pair(a,b int) (int,bool) { trace+="m";runtime.Gosched();return a*10+b,true }
func direct() int { return consume(produce()) }
func plainReturn() int { return plain(produce()) }
func forward() (int,bool) { return pair(produce()) }
func named() (n int,ok bool) { defer func(){n++;trace+="d"}();return pair(produce()) }
func condition() int { if predicate(produce()) { return 7 };return 9 }
func nested() int { return consume(plain(produce()),4) }
func statement() { consume(produce()) }
func loopBody() int { total:=0;for i:=0;i<3;i++ { total=total+consume(produce()) };return total }
func recursive(n int) int { if n==0 {return consume(produce())};return recursive(n-1) }
func chooseConsumer(which bool) func(int,int)int { trace+="s";runtime.Gosched();if which {return alternate};return consume }
func selected(which bool) int { return chooseConsumer(which)(produce()) }
func triple() (int,int,int) { trace+="t";runtime.Gosched();return 1,2,3 }
func variadic(a int,rest ...int) int { trace+="v";runtime.Gosched();return a+rest[0]*10+rest[1]*100 }
func variadicReturn() int { return variadic(triple()) }
func recovered() (int,int) { defer func(){recover()}();trace+="r";runtime.Gosched();panic("recover producer") }
func recoveredReturn() int { return consume(recovered()) }
func panicProducer() (int,int) { trace+="x";runtime.Gosched();panic("producer") }
func panicReturn() (v any) { defer func(){if x:=recover();x!=nil {v=x}}();return consume(panicProducer()) }
func panicConsumer(a,b int) int { trace+="y";runtime.Gosched();panic(a*10+b) }
func consumerPanic() (v any) { defer func(){if x:=recover();x!=nil {v=x}}();return panicConsumer(produce()) }
var exitDone chan int
func quit(a,b int) int { trace+="q";runtime.Gosched();exitDone<-a*10+b;runtime.Goexit();return 0 }
func exitReturn() int { defer func(){close(exitDone)}();return quit(produce()) }
var gcRequest=make(chan bool)
var gcAck=make(chan bool)
func pointers() (*int,*int) { a,b:=40,2;runtime.Gosched();return &a,&b }
func pointerConsumer(a,b *int) (*int,int) { runtime.Gosched();gcRequest<-true;<-gcAck;return a,*a+*b }
func pointerReturn() (*int,int) { return pointerConsumer(pointers()) }
// These prefixes remain ordinary fallback and must keep their evaluation order.
func shortCircuit(flag bool) bool { return flag && predicate(produce()) }
func prefix() int { trace+="f";return 100 }
func observablePrefix() int { return prefix()+consume(produce()) }
func missing(a,b int) int { trace+="z";runtime.Gosched();goto end;end:return a+b }
func missingReturn() int { return missing(produce()) }
func unknown(input func(int,int)int) int { return input(produce()) }
func main() {
 trace="";if direct()!=23 || trace!="pc" {panic("direct "+trace)}
 trace="";if plainReturn()!=5 || trace!="pl" {panic("plain "+trace)}
 trace="";a,b:=forward();if a!=23 || !b || trace!="pm" {panic("forward "+trace)}
 trace="";a,b=named();if a!=24 || !b || trace!="pmd" {panic("named "+trace)}
 trace="";if condition()!=7 || trace!="pb" {panic("condition "+trace)}
 trace="";if nested()!=54 || trace!="plc" {panic("nested "+trace)}
 trace="";statement();if trace!="pc" {panic("statement "+trace)}
 trace="";if loopBody()!=69 || trace!="pcpcpc" {panic("loop "+trace)}
 trace="";if recursive(500)!=23 || trace!="pc" {panic("recursion "+trace)}
 trace="";if selected(false)!=23 || trace!="spc" {panic("selected "+trace)}
 trace="";if selected(true)!=203 || trace!="spa" {panic("selected alternate "+trace)}
 trace="";if variadicReturn()!=321 || trace!="tv" {panic("variadic "+trace)}
 trace="";if recoveredReturn()!=0 || trace!="rc" {panic("recovered zero "+trace)}
 trace="";if panicReturn()!="producer" || trace!="x" {panic("producer panic "+trace)}
 trace="";if consumerPanic()!=23 || trace!="py" {panic("consumer panic "+trace)}
 trace="";exitDone=make(chan int,1);go exitReturn();if <-exitDone!=23 {panic("Goexit argument")};if _,ok:=<-exitDone;ok || trace!="pq" {panic("Goexit cleanup "+trace)}
 go func(){<-gcRequest;runtime.GC();gcAck<-true}();p,n:=pointerReturn();if *p!=40 || n!=42 {panic("pointer argument lifetime")}
 trace="";if shortCircuit(false) || trace!="" {panic("short circuit false "+trace)};if !shortCircuit(true) || trace!="pb" {panic("short circuit true "+trace)}
 trace="";if observablePrefix()!=123 || trace!="fpc" {panic("observable prefix "+trace)}
 trace="";if missingReturn()!=5 || trace!="pz" {panic("missing factory "+trace)}
 trace="";if unknown(alternate)!=203 || trace!="pa" {panic("unknown "+trace)}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"shortCircuit", "nested await site"}, {"observablePrefix", "nested await site"}, {"missingReturn", "unsupported coroutine dependency main.missing"}, {"unknown", "dynamic await site"}},
		[]string{"direct", "plainReturn", "forward", "named", "condition", "nested", "statement", "loopBody", "recursive", "selected", "variadicReturn", "recoveredReturn", "panicReturn", "consumerPanic", "exitReturn", "pointerReturn"})
}

func TestTupleCallInitializersImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/coroarg\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func Produce() (int,int) {runtime.Gosched();return 4,5}
func Consume(a,b int) (n int,ok bool) {defer func(){n++}();runtime.Gosched();return a*10+b,true}
//go:noinline
func Missing(a,b int) int {runtime.Gosched();goto end;end:return a+b}
`,
		"main.go": `package main
import ("runtime";"example.com/coroarg/leaf")
func consume(a,b int) int {runtime.Gosched();return a*10+b}
func produce() (int,int) {runtime.Gosched();return 2,3}
func importedProducer() int {return consume(leaf.Produce())}
func importedConsumer() (int,bool) {return leaf.Consume(produce())}
func importedBoth() (int,bool) {return leaf.Consume(leaf.Produce())}
func missing() int {return leaf.Missing(produce())}
func main(){if importedProducer()!=45 {panic("imported producer")};a,b:=importedConsumer();if a!=24 || !b {panic("imported consumer")};a,b=importedBoth();if a!=46 || !b || missing()!=5 {panic("imported tuple calls")};println("switch-return-ok")}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/coroarg/...=",
		[]switchFallback{{"missing", "unsupported coroutine dependency example.com/coroarg/leaf.Missing"}},
		[]string{"importedProducer", "importedConsumer", "importedBoth"})
}
