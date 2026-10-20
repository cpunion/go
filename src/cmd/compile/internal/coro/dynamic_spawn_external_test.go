// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestDynamicSpawn(t *testing.T) {
	const source = `package main
import (
 "runtime"
 "runtime/debug"
)

type box struct { value int }
type worker func(*box,chan int,chan struct{},chan int)(int,*box)
func first(p *box, ready chan int, gate chan struct{}, done chan int) (int, *box) {
 runtime.Gosched()
 ready <- p.value
 <-gate
 done <- p.value
 return p.value, p
}
func second(p *box, ready chan int, gate chan struct{}, done chan int) (n int, result *box) {
 defer func(){n++}()
 runtime.Gosched()
 ready <- p.value
 <-gate
 done <- p.value+1
 return p.value, p
}
func spawn(ready chan int, gate chan struct{}, done chan int) {
 for i := range 128 {
  f := first
  if i%2 != 0 {f=second}
  alias := worker(f)
  go alias(&box{i}, ready, gate, done)
 }
}
func plain(done chan int, n int) int {done <- n; return n}
func yielding(done chan int, n int) int {runtime.Gosched(); done <- n+10; return n}
func mixed(which bool, done chan int) {
 f:=plain
 if which {f=yielding}
 go f(done, 7)
}
func plainZero() {}
func yieldZero() {runtime.Gosched()}
func zero(which bool) {f:=plainZero;if which {f=yieldZero};go f()}
func variadicA(done chan int, values ...int) {runtime.Gosched(); done <- values[0]+values[1]}
func variadicB(done chan int, values ...int) {runtime.Gosched(); done <- 2*(values[0]+values[1])}
func variadic(which bool, done chan int) {
 f:=variadicA
 if which {f=variadicB}
 values:=[]int{2,3}
 go f(done,values...)
}
func choose(which bool) func(chan int,int)int {
 runtime.Gosched()
 if which {return yielding}
 return plain
}
func returned(which bool, done chan int) {f:=choose(which);go f(done,9)}
var arguments int
func argument() int {runtime.Gosched();arguments++;return 11}
func argumentSpawn(done chan int) {f:=yielding;go f(done,argument())}
func panicArgument() int {runtime.Gosched();panic(42)}
func argumentPanic(nilTarget bool, done chan int) (result any) {
 defer func(){result=recover()}()
 f:=yielding
 if nilTarget {f=nil}
 go f(done,panicArgument())
 return nil
}
func exitArgument(mark *int) int {defer func(){*mark=1}();runtime.Gosched();runtime.Goexit();return 0}
func closeDone(done chan struct{}) {close(done)}
func argumentExit(done chan int, exited chan struct{}, mark *int) {
 defer closeDone(exited)
 f:=yielding
 go f(done,exitArgument(mark))
 done <- 1000
}
func childExit(done chan int) {runtime.Gosched();done <- 3;runtime.Goexit()}
func childFinish(done chan int) {runtime.Gosched();done <- 4}
func exiting(which bool, done chan int) {
 f:=childFinish
 if which {f=childExit}
 go f(done)
 done <- 5
}
func recursive(depth int, done chan int) {
 if depth==0 {done <- 6;return}
 f:=recursive
 go f(depth-1,done)
}
// Open targets and caller cells that a source closure can mutate stay ordinary.
var global=yielding
func parameter(f func(chan int,int)int, done chan int) {go f(done,1)}
func globalSpawn(done chan int) {go global(done,2)}
func captured(done chan int) {
 f:=plain
 change:=func(){f=yielding}
 change()
 go f(done,3)
}
func addressed(done chan int) {f:=plain;p:=&f;*p=yielding;go f(done,4)}
func missing(done chan int, n int) int {runtime.Gosched();goto end;end:done <- n;return n}
func missingFactory(which bool, done chan int) {f:=yielding;if which {f=missing};go f(done,8)}
func main() {
 native:=false
 if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 baseline:=runtime.NumGoroutine()
 finished:=make(chan struct{})
 go func(){spawn(ready,gate,done);close(finished)}()
 <-finished
 var seen [128]bool
 for range 128 {n:=<-ready;if n<0 || n>=128 || seen[n] {panic("frozen argument")};seen[n]=true}
 if native && runtime.NumGoroutine()>baseline+12 {panic("dynamic spawn retained ordinary goroutines")}
 runtime.GC()
 close(gate)
 sum:=0
 for range 128 {sum+=<-done}
 if sum!=8192 {panic("independent discarded results or roots")}
 mixed(false,done);if <-done!=7 {panic("plain factory")}
 mixed(true,done);if <-done!=17 {panic("mixed factory")}
 zero(false);zero(true)
 variadic(false,done);if <-done!=5 {panic("variadic")}
 variadic(true,done);if <-done!=10 {panic("variadic second")}
 returned(false,done);if <-done!=9 {panic("returned plain")}
 returned(true,done);if <-done!=19 {panic("returned yielding")}
 argumentSpawn(done);if <-done!=21 || arguments!=1 {panic("argument continuation")}
 if argumentPanic(false,done)!=42 || argumentPanic(true,done)!=42 {panic("parent argument panic")}
 exited,mark:=make(chan struct{}),0
 go argumentExit(done,exited,&mark)
 <-exited
 if mark!=1 || len(done)!=0 {panic("parent argument Goexit")}
 exiting(true,done)
 a,b:=<-done,<-done
 if a+b!=8 {panic("child Goexit terminated parent")}
 exiting(false,done)
 a,b=<-done,<-done
 if a+b!=9 {panic("child normal return")}
 recursive(256,done);if <-done!=6 {panic("recursive spawn")}
 parameter(yielding,done);if <-done!=11 {panic("parameter fallback")}
 globalSpawn(done);if <-done!=12 {panic("global fallback")}
 captured(done);if <-done!=13 {panic("captured fallback")}
 addressed(done);if <-done!=14 {panic("addressed fallback")}
 missingFactory(true,done);if <-done!=8 {panic("missing factory fallback")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"parameter", "dynamic spawn site"}, {"globalSpawn", "dynamic spawn site"},
			{"captured", "dynamic spawn site"}, {"addressed", "dynamic spawn site"},
			{"missingFactory", "unsupported coroutine dependency main.missing"},
		},
		[]string{"spawn", "mixed", "zero", "plainZero", "variadic", "returned", "argumentSpawn", "argumentPanic", "argumentExit", "exiting", "recursive"})
}

func TestDynamicSpawnFatal(t *testing.T) {
	const source = `package main
import (
 "context"
 "os"
 "os/exec"
 "runtime"
 "strings"
 "time"
)
func zero() {runtime.Gosched()}
func value(n int) int {runtime.Gosched();return n}
func failing(n int) int {runtime.Gosched();panic("dynamic-child-panic")}
func argument() int {println("argument-once");runtime.Gosched();return 1}
func nilZero(which bool) {f:=zero;if which {f=nil};go f()}
func nilValue(which bool) {f:=value;if which {f=nil};go f(argument())}
func panicChild(which bool) {f:=value;if which {f=failing};go f(argument())}
func main() {
 if len(os.Args)>1 {
  defer func(){if recover()!=nil {println("parent-recovered-child")}}()
  switch os.Args[1] {
  case "zero":nilZero(true)
  case "value":nilValue(true)
  case "panic":panicChild(true)
  }
  select {}
 }
 exe,err:=os.Executable();if err!=nil {panic(err)}
 for _,mode:=range []string{"zero","value","panic"} {
  ctx,cancel:=context.WithTimeout(context.Background(),2*time.Second)
  data,err:=exec.CommandContext(ctx,exe,mode).CombinedOutput()
  expired:=ctx.Err()!=nil
  cancel()
  out:=string(data)
  want:="invalid memory address or nil pointer dereference"
  if mode=="zero" {want="go of nil func value"}
  if mode=="panic" {want="dynamic-child-panic"}
  if expired || err==nil || !strings.Contains(out,want) || strings.Contains(out,"parent-recovered-child") {panic("child failure semantics: "+mode+": "+out)}
  count:=0
  for _,line:=range strings.Split(out,"\n") {if line=="argument-once" {count++}}
  if (mode=="zero" && count!=0) || (mode!="zero" && count!=1) {panic("child argument evaluation: "+out)}
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"nilZero", "nilValue", "panicChild"})
}

func TestDynamicSpawnImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/dynamicspawn\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
func First(done chan int,n int) int {runtime.Gosched();done <- n+1;return n}
func Second(done chan int,n int) int {defer func(){n++}();runtime.Gosched();done <- n+2;return n}
func Plain(done chan int,n int) int {return n+3}
`,
		"main.go": `package main
import "example.com/dynamicspawn/leaf"
func spawn(which bool,done chan int) {f:=leaf.First;if which {f=leaf.Second};go f(done,10)}
func missing(which bool,done chan int) {f:=leaf.First;if which {f=leaf.Plain};go f(done,20)}
func main() {
 done:=make(chan int,4)
 spawn(false,done);if <-done!=11 {panic("import first")}
 spawn(true,done);if <-done!=12 {panic("import second")}
 missing(true,done)
 missing(false,done);if <-done!=21 {panic("missing imported factory")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/dynamicspawn/...=",
		[]switchFallback{{"missing", "unsupported coroutine dependency example.com/dynamicspawn/leaf.Plain"}},
		[]string{"spawn"})
}
