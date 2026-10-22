// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClosedParameterCalls(t *testing.T) {
	const source = `package main
import "runtime"
func first(x int) int {runtime.Gosched();return x+10}
func second(x int) int {runtime.Gosched();return x+20}
func plain(x int) int {return x+30}
//go:noinline
func invoke(f func(int)int,x int) int {a:=f;b:=a;v:=b(x);return v}
//go:noinline
func hop(f func(int)int,x int) int {v:=invoke(f,x);return v+1}
//go:noinline
func recurse(f func(int)int,n int) int {if n==0 {v:=f(3);return v};v:=recurse(f,n-1);return v+1}
//go:noinline
func intentionallyLongFunctionNameForIndependentSourceBodies(f func(int)int,x int) int {v:=f(x);return v}
func driver(which bool) int {f:=first;if which {f=second};v:=hop(f,2);return v}
func repeated(which bool) int {f:=first;if which {f=second};a:=invoke(f,1);b:=invoke(f,2);return a+b}
func recursiveDriver() int {v:=recurse(second,64);return v}
func longDriver() int {v:=intentionallyLongFunctionNameForIndependentSourceBodies(first,1);return v}
func plainDriver(which bool) int {f:=first;if which {f=plain};v:=invoke(f,8);return v}
func allPlain() int {v:=invoke(plain,5);return v}
var arguments int
func argument() int {arguments++;return 7}
func nilDriver(which bool) (result any) {
 defer func(){if x:=recover();x!=nil {result=x}}()
 f:=first;if which {f=nil};v:=invoke(f,argument());return v
}
type box struct {value int}
var gcRequest=make(chan bool)
var gcAck=make(chan bool)
func pairA(p *box) (*box,int) {runtime.Gosched();gcRequest<-true;<-gcAck;return p,p.value}
func pairB(p *box) (q *box,n int) {defer func(){n++}();runtime.Gosched();gcRequest<-true;<-gcAck;return p,p.value+10}
//go:noinline
func applyPair(f func(*box)(*box,int),p *box) (q *box,n int) {defer func(){n++}();a:=f;q,n=a(p);return}
func pairDriver(which bool) int {f:=pairA;if which {f=pairB};p:=&box{40};q,n:=applyPair(f,p);runtime.Gosched();if p!=q || q.value!=40 {panic("parameter pointer lifetime")};return n}
//go:noinline
func variadic(f func(...int)int,values ...int) int {n:=f(values...);return n}
func sum(values ...int) int {runtime.Gosched();n:=0;for i:=range len(values) {n+=values[i]};return n}
func variadicDriver() int {v:=variadic(sum,2,3);return v}
func panicking(x int) int {runtime.Gosched();panic(x)}
func panicDriver() (result int) {defer func(){if x:=recover();x!=nil {result=x.(int)}}();v:=invoke(panicking,71);return v}
var exitDone chan struct{}
var exitMark int
func exiting() {runtime.Gosched();runtime.Goexit()}
//go:noinline
func applyVoid(f func()) {defer func(){exitMark++}();f();panic("continued after callback Goexit")}
func exitWorker() {defer close(exitDone);applyVoid(exiting);panic("continued after parameter Goexit")}
func main(){
 if driver(false)!=13 || driver(true)!=23 || repeated(false)!=23 || repeated(true)!=43 || recursiveDriver()!=87 || longDriver()!=11 {panic("closed parameter calls")}
 if plainDriver(false)!=18 || plainDriver(true)!=38 || allPlain()!=35 {panic("plain callback factory")}
 if nilDriver(false)!=17 || arguments!=1 {panic("parameter argument evaluation")}
 if _,ok:=nilDriver(true).(runtime.Error);!ok || arguments!=2 {panic("nil parameter argument order")}
 go func(){for range 2 {<-gcRequest;runtime.GC();gcAck<-true}}()
 if pairDriver(false)!=41 || pairDriver(true)!=52 || variadicDriver()!=5 {panic("parameter results or variadic")}
 if panicDriver()!=71 {panic("parameter panic propagation")}
 exitDone=make(chan struct{});go exitWorker();<-exitDone;if exitMark!=1 {panic("parameter Goexit cleanup")}
 runtime.LockOSThread();if driver(true)!=23 {panic("thread-bound parameter fallback")};runtime.UnlockOSThread()
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"invoke", "dynamic await site"}, {"recurse", "dynamic await site"}},
		[]string{"driver", "repeated", "recursiveDriver", "longDriver", "plainDriver", "allPlain", "nilDriver", "pairDriver", "variadicDriver", "panicDriver", "exitWorker"})
}

func TestClosedParameterOrdinaryBoundaries(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/coroparam\n\ngo 1.28\n",
		"bridge/bridge.go": `package bridge
import _ "unsafe"
//go:linkname Open main.invoke
func Open(f func(int)int,x int) int
`,
		"leaf/leaf.go": `package leaf
import "runtime"
func First(x int) int {runtime.Gosched();return x+40}
func Open(f func(int)int,x int) int {v:=f(x);return v}
`,
		"main.go": `package main
import (
 "runtime"
 "example.com/coroparam/bridge"
 "example.com/coroparam/leaf"
)
func first(x int) int {runtime.Gosched();return x+10}
func outside(x int) int {runtime.Gosched();return x+90}
//go:noinline
func invoke(f func(int)int,x int) int {v:=f(x);return v}
func closed() int {v:=invoke(first,2);return v}
func importedDriver(which bool) int {f:=leaf.First;if which {f=first};v:=invoke(f,4);return v}
func importedOpen() int {v:=leaf.Open(first,5);return v}
//go:noinline
func inspectionInvoke(f func()) {f()}
func inspect() {pc,_,_,ok:=runtime.Caller(1);if !ok || runtime.FuncForPC(pc).Name()!="main.inspectionInvoke" {panic("ordinary source frame changed")}}
var recovered int
func recoverDirect() {if recover()!=nil {recovered++}}
func rejected() {
 defer recoverDirect()
 inspectionInvoke(inspect)
 goto end
 end:panic("ordinary recovery")
}
//go:noinline
func mutable(f func(int)int,x int) int {f=outside;v:=f(x);return v}
//go:noinline
func addressed(f func(int)int,x int) int {p:=&f;*p=outside;v:=f(x);return v}
//go:noinline
func captured(f func(int)int,x int) int {change:=func(){f=outside};change();v:=f(x);return v}
func negatives() int {a:=mutable(first,1);b:=addressed(first,2);c:=captured(first,3);return a+b+c}
func main(){
 if closed()!=12 || bridge.Open(outside,3)!=93 || negatives()!=276 || importedDriver(false)!=44 || importedDriver(true)!=14 || importedOpen()!=15 {panic("open parameter boundary")}
 rejected();if recovered!=1 {panic("ordinary direct recover")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/coroparam/...=",
		[]switchFallback{{"invoke", "dynamic await site"}, {"rejected", "control flow goto"}},
		[]string{"closed", "importedDriver"})
}

func TestClosedParameterParked(t *testing.T) {
	const source = `package main
import (
 "runtime"
 "runtime/debug"
)
type box struct {value int}
func first(p *box,ready chan int,gate chan struct{},done chan int) {runtime.Gosched();ready<-p.value;<-gate;done<-p.value}
func second(p *box,ready chan int,gate chan struct{},done chan int) {runtime.Gosched();ready<-p.value;<-gate;done<-p.value+1}
//go:noinline
func apply(f func(*box,chan int,chan struct{},chan int),p *box,ready chan int,gate chan struct{},done chan int) {a:=f;a(p,ready,gate,done)}
func worker(n int,ready chan int,gate chan struct{},done chan int) {f:=first;if n%2!=0 {f=second};apply(f,&box{n},ready,gate,done)}
func spawn(ready chan int,gate chan struct{},done chan int) {for n:=range 128 {go worker(n,ready,gate,done)}}
func main(){
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 baseline:=runtime.NumGoroutine();finished:=make(chan struct{})
 go func(){spawn(ready,gate,done);close(finished)}();<-finished
 var seen [128]bool
 for range 128 {n:=<-ready;if n<0 || n>=128 || seen[n] {panic("parameter child argument")};seen[n]=true}
 if native && runtime.NumGoroutine()>baseline+12 {panic("closed parameter await used ordinary goroutines")}
 runtime.GC();close(gate);total:=0;for range 128 {total+=<-done};if total!=8192 {panic("parameter parked task result")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"worker", "spawn"})
}

func TestClosedParameterWaitGroupGo(t *testing.T) {
	const source = `package main
import (
 "runtime"
 "runtime/debug"
 "sync"
)
var group sync.WaitGroup
var ready=make(chan int,128)
var gate=make(chan struct{})
func first() {runtime.Gosched();ready<-1;<-gate}
func second() {runtime.Gosched();ready<-2;<-gate}
//go:noinline
func apply(wg *sync.WaitGroup,f func()) {wg.Go(f)}
func spawn(which bool) {f:=first;if which {f=second};apply(&group,f)}
func main(){
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 baseline:=runtime.NumGoroutine()
 for n:=range 128 {spawn(n%2!=0)}
 total:=0;for range 128 {total+=<-ready};if total!=192 {panic("parameter WaitGroup.Go target")}
 if native && runtime.NumGoroutine()>baseline+12 {panic("parameter WaitGroup.Go fell back")}
 runtime.GC();close(gate);group.Wait()
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"spawn"})
}

func TestClosedParameterOperations(t *testing.T) {
	const source = `package main
import (
 "runtime"
 "runtime/debug"
)
var ready=make(chan int,128)
var gate=make(chan struct{})
var done=make(chan int,128)
func first() {runtime.Gosched();ready<-1;<-gate;done<-1}
func second() {runtime.Gosched();ready<-2;<-gate;done<-2}
//go:noinline
func applyGo(f func()) {go f()}
func spawn(which bool) {f:=first;if which {f=second};applyGo(f)}
var trace int
func cleanupA() {trace=trace*10+1}
func cleanupB() {trace=trace*10+2}
//go:noinline
func applyDefer(f func()) {defer f();runtime.Gosched();trace=trace*10+3}
func deferred(which bool) {f:=cleanupA;if which {f=cleanupB};applyDefer(f)}
var recovered any
func recoverA() {recovered=recover()}
func recoverB() {if x:=recover();x!=nil {recovered=x}}
//go:noinline
func applyRecover(f func()) {defer f();runtime.Gosched();panic("parameter-defer-panic")}
func recovering(which bool) {f:=recoverA;if which {f=recoverB};applyRecover(f)}
func main(){
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 baseline:=runtime.NumGoroutine();for n:=range 128 {spawn(n%2!=0)}
 total:=0;for range 128 {total+=<-ready};if total!=192 {panic("parameter go target")}
 if native && runtime.NumGoroutine()>baseline+12 {panic("parameter go fell back")}
 runtime.GC();close(gate);total=0;for range 128 {total+=<-done};if total!=192 {panic("parameter go completion")}
 deferred(false);if trace!=31 {panic("parameter deferred call")};trace=0;deferred(true);if trace!=32 {panic("parameter deferred selection")}
 recovering(false);if recovered!="parameter-defer-panic" {panic("parameter direct recover")};recovered=nil;recovering(true);if recovered!="parameter-defer-panic" {panic("parameter recover selection")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"spawn", "deferred", "recovering"})
}

func TestClosedParameterReturnedTargets(t *testing.T) {
	const source = `package main
import "runtime"
func first(x int) int {runtime.Gosched();return x+10}
func second(x int) int {runtime.Gosched();return x+20}
//go:noinline
func identity(f func(int)int) func(int)int {a:=f;return a}
//go:noinline
func tuple(f func(int)int,n int) (g func(int)int,v int) {defer func(){v++}();g,v=f,n;return}
//go:noinline
func invoke(f func(int)int,x int) int {v:=f(x);return v}
func returned(which bool) int {f:=first;if which {f=second};g:=identity(f);v:=invoke(g,3);return v}
func paired(which bool) int {f:=first;if which {f=second};g,n:=tuple(f,4);v:=invoke(g,n);return v}
func tupleArgument(which bool) int {f:=first;if which {f=second};v:=invoke(tuple(f,5));return v}
func main(){
 if returned(false)!=13 || returned(true)!=23 || paired(false)!=15 || paired(true)!=25 || tupleArgument(false)!=16 || tupleArgument(true)!=26 {panic("returned parameter proof")}
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"invoke", "dynamic await site"}},
		[]string{"returned", "paired", "tupleArgument"})
}

func TestClosedParameterContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package paramcontext
import (
 "runtime"
 "sync"
 "testing"
 "testing/synctest"
 "time"
)
var value int
func slow() {time.Sleep(time.Hour);value=1}
func fast() {runtime.Gosched();value=2}
//go:noinline
func apply(wg *sync.WaitGroup,f func()) {wg.Go(f)}
func spawnSlow(wg *sync.WaitGroup) {apply(wg,slow)}
func spawnFast(wg *sync.WaitGroup) {apply(wg,fast)}
func TestBubble(t *testing.T) {
 for range 1 {
  synctest.Test(t,func(t *testing.T){
   start:=time.Now()
   var wg sync.WaitGroup
   spawnSlow(&wg);wg.Wait()
   if value!=1 || time.Since(start)!=time.Hour {t.Fatal("callback lost ordinary synctest accounting")}
   synctest.Wait()
  })
 }
}
func TestThread(t *testing.T) {
 for range 1 {
  runtime.LockOSThread()
  defer runtime.UnlockOSThread()
  var wg sync.WaitGroup
  spawnFast(&wg);wg.Wait()
  if value!=2 {t.Fatal("thread-bound callback fallback")}
 }
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":          "module example.com/paramcontext\n\ngo 1.28\n",
		"context_test.go": source,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"inline", "noinline", "race", "nocoro"} {
		t.Run(mode, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "contexts.exe")
			args := []string{"test", "-c", "-o", exe}
			experiment := "coro"
			flags := "-d=coro=4"
			if mode == "noinline" {
				flags = "-l " + flags
			}
			if mode == "race" {
				testenv.MustHaveCGO(t)
				args = append(args, "-race")
			}
			if mode == "nocoro" {
				experiment, flags = "nocoro", "-l"
			}
			args = append(args, "-gcflags=example.com/paramcontext="+flags)
			cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
			cmd.Dir = dir
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			cmd = testenv.Command(t, exe, "-test.timeout=10s")
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("contexts: %v\n%s", err, data)
			}
			cmd = testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
			data, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"spawnFast", "spawnSlow"} {
				present := strings.Contains(string(data), "example.com/paramcontext."+name+".coro")
				if present != (mode == "inline" || mode == "noinline") {
					t.Fatalf("%s: native factory present=%v", name, present)
				}
			}
		})
	}
}
