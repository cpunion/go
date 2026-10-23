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

func TestClosedParameterNestedDefer(t *testing.T) {
	const source = `package main
import ("runtime";"runtime/debug")
type state struct {trace []int}
func record(p *state,n int) {p.trace=append(p.trace,n)}
func first(p *state,n int) {p.trace=append(p.trace,n)}
func second(p *state,n int) {p.trace=append(p.trace,n+10)}
//go:noinline
func apply(f func(*state,int),p *state,n int) {
 defer record(p,n+100)
 for i:=range 3 {defer record(p,n+200+i)}
 f(p,n)
}
var arguments int
func argument() int {arguments++;return arguments}
func repeated(p *state) {
 for i:=range 4 {f:=first;if i%2!=0 {f=second};defer apply(f,p,argument());f=nil}
 runtime.Gosched()
}
func parked(p *state,ready chan int,gate,done chan struct{}) {
 defer func(){close(done)}()
 defer apply(first,p,7)
 ready<-1
 <-gate
}
func start(p *state,ready chan int,gate,done chan struct{}) {go parked(p,ready,gate,done)}
func equal(p *state,want ...int) {if len(p.trace)!=len(want) {panic("trace length")};for i,v:=range want {if p.trace[i]!=v {panic("nested order or snapshot")}}}
func main(){
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 p:=new(state);repeated(p);if arguments!=4 {panic("argument count")}
 equal(p,14,206,205,204,104,3,205,204,203,103,12,204,203,202,102,1,203,202,201,101)
 baseline:=runtime.NumGoroutine();ready,gate:=make(chan int,128),make(chan struct{});ps:=make([]*state,128);ds:=make([]chan struct{},128)
 for i:=range 128 {p,done:=new(state),make(chan struct{});ps[i],ds[i]=p,done;start(p,ready,gate,done)}
 for range 128 {<-ready};if native && runtime.NumGoroutine()>baseline+12 {panic("ordinary nested cleanup tasks")};runtime.GC();close(gate)
 for i:=range 128 {<-ds[i];equal(ps[i],7,209,208,207,107)}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"repeated", "parked", "start"})
}

func TestClosedParameterNestedDeferTerminal(t *testing.T) {
	const source = `package main
import "runtime"
type state struct {trace []int;seen any;direct any}
func record(p *state,n int) {p.trace=append(p.trace,n)}
func plain(p *state,n int) {p.trace=append(p.trace,n)}
func failing(p *state,n int) {record(p,n);panic(n)}
func catch(p *state) {p.seen=recover();record(p,30)}
func boom(p *state) {record(p,40);panic("nested")}
func exit(p *state) {record(p,50);runtime.Goexit()}
//go:noinline
func apply(f func(*state,int),p *state,n int) {
 defer record(p,20)
 defer catch(p)
 f(p,n)
}
func outer(p *state,which bool) (result any) {
 defer func(){result=recover()}()
 f:=plain;if which {f=failing}
 defer apply(f,p,7)
 runtime.Gosched()
 panic("outer")
}
//go:noinline
func directApply(f func(*state,int),p *state) {
 defer catch(p)
 p.direct=recover()
 f(p,8)
}
func direct(p *state) {defer directApply(plain,p);runtime.Gosched();panic("direct")}
//go:noinline
func replacementApply(f func(*state,int),p *state) {defer boom(p);f(p,9)}
func replacement(p *state) (result any) {defer func(){result=recover()}();defer replacementApply(failing,p);runtime.Gosched();panic("outer")}
//go:noinline
func exitApply(f func(*state,int),p *state) {defer record(p,60);defer exit(p);f(p,10)}
func exiting(p *state,done chan struct{}) {defer func(){close(done)}();defer func(){p.direct=recover()}();defer exitApply(failing,p);runtime.Gosched();panic("outer")}
var zeroSeen any
func zeroPlain() {}
func zeroPanic() {panic("zero")}
func zeroRecover() {zeroSeen=recover()}
//go:noinline
func deferredOnly(f func()) {defer f()}
func dynamic(which int) (result any) {
 defer func(){result=recover()}()
 f:=zeroPlain;switch which {case 1:f=zeroPanic;case 2:f=zeroRecover;case 3:f=nil}
 defer deferredOnly(f)
 f=nil
 runtime.Gosched()
 panic("outer")
}
func equal(p *state,want ...int) {if len(p.trace)!=len(want) {panic("trace length")};for i,v:=range want {if p.trace[i]!=v {panic("terminal nested order")}}}
func main(){
 p:=new(state);if outer(p,false)!="outer" || p.seen!=nil {panic("inner recovered outer panic")};equal(p,7,30,20)
 p=new(state);if outer(p,true)!="outer" || p.seen!=7 {panic("nested callback panic ownership")};equal(p,7,30,20)
 p=new(state);direct(p);if p.direct!="direct" || p.seen!=nil {panic("direct then nested recover")};equal(p,8,30)
 p=new(state);if replacement(p)!="nested" {panic("nested replacement")};equal(p,9,40)
 p=new(state);done:=make(chan struct{});go exiting(p,done);<-done;if p.direct!=nil {panic("nested Goexit ownership")};equal(p,10,50,60)
 if dynamic(0)!="outer" || dynamic(1)!="zero" || dynamic(2)!="outer" || zeroSeen!=nil {panic("deferred callback recover ownership")}
 if _,ok:=dynamic(3).(runtime.Error);!ok {panic("nested delayed nil")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"outer", "direct", "replacement", "exiting", "dynamic"})
}

func TestClosedParameterNestedDeferBoundaries(t *testing.T) {
	const source = `package main
import "runtime"
var trace []int
func record(n int) {trace=append(trace,n)}
func plain(n int) {record(n)}
func slow() {runtime.Gosched();record(90)}
func spawn() {go zero()}
func zero() {}
//go:noinline
func slowApply(f func(int)) {defer slow();f(1)}
//go:noinline
func spawnApply(f func(int)) {defer spawn();f(2)}
func suspending() {defer slowApply(plain);runtime.Gosched()}
func creating() {defer spawnApply(plain);runtime.Gosched()}
//go:noinline
func capturedApply(f func(int)) {defer f(3);f(4)}
func captured() {defer capturedApply(plain);runtime.Gosched()}
//go:noinline
func unsupported() {goto end;end:panic("unsupported")}
//go:noinline
func missingApply(f func()) {defer f()}
func missing() (result any) {defer func(){result=recover()}();defer missingApply(unsupported);runtime.Gosched();return}
//go:noinline
func inspection(f func()) {defer zero();f()}
func inspect() {pc,_,_,ok:=runtime.Caller(1);if !ok || runtime.FuncForPC(pc).Name()!="main.inspection" {panic("source body not restored")}}
func rejected() {defer inspection(inspect);goto end;end:runtime.Gosched()}
func main(){suspending();creating();captured();if missing()!="unsupported" {panic("missing factory fallback")};rejected();want:=[]int{1,90,2,4,3};if len(trace)!=len(want) {panic("fallback length")};for i,v:=range want {if trace[i]!=v {panic("fallback order")}};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, []switchFallback{{"suspending", "suspending defer"}, {"creating", "defer target has execution constraints task-context"}, {"rejected", "control flow goto"}}, nil)
}

func TestClosedParameterNestedDeferContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package paramnesteddefercontext
import (
 "runtime"
 "testing"
 "testing/synctest"
 "time"
)
var value int
func plain() {value++}
func cleanup() {value+=10}
//go:noinline
func apply(f func()) {defer cleanup();f()}
func deferredSlow() {defer apply(plain);time.Sleep(time.Hour)}
func deferredFast() {defer apply(plain);runtime.Gosched()}
func TestBubble(t *testing.T) {
 for range 1 {
  synctest.Test(t,func(t *testing.T){
   start:=time.Now()
   value=0
   deferredSlow()
   if value!=11 || time.Since(start)!=time.Hour {t.Fatal("callback lost ordinary synctest accounting")}
   synctest.Wait()
  })
 }
}
func TestThread(t *testing.T) {
 for range 1 {
  runtime.LockOSThread()
  defer runtime.UnlockOSThread()
  value=0
  deferredFast()
  if value!=11 {t.Fatal("thread-bound callback fallback")}
 }
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":          "module example.com/paramnesteddefercontext\n\ngo 1.28\n",
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
			args = append(args, "-gcflags=example.com/paramnesteddefercontext="+flags)
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
			for _, name := range []string{"deferredFast", "deferredSlow"} {
				present := false
				for _, line := range strings.Split(string(data), "\n") {
					fields := strings.Fields(line)
					if len(fields) == 3 && (fields[1] == "T" || fields[1] == "t") && fields[2] == "example.com/paramnesteddefercontext."+name+".coro" {
						present = true
					}
				}
				if present != (mode == "inline" || mode == "noinline") {
					t.Fatalf("%s: native factory present=%v", name, present)
				}
			}
		})
	}
}

func TestClosedParameterNestedDeferImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/paramnesteddefer\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
var Seen any
var Trace []int
func Catch() {Seen=recover()}
func Record(n int) {Trace=append(Trace,n)}
func Plain(n int) int {if n<0 {panic(n)};return n}
func Fail(n int) int {panic(n)}
`,
		"main.go": `package main
import ("runtime";"example.com/paramnesteddefer/leaf")
//go:noinline
func apply(f func(int)int,n int) {defer leaf.Record(20);defer leaf.Catch();f(n)}
func imported(which bool) (result any) {defer func(){result=recover()}();f:=leaf.Plain;if which {f=leaf.Fail};defer apply(f,7);runtime.Gosched();panic("outer")}
func main(){if imported(false)!="outer" || leaf.Seen!=nil {panic("imported outer ownership")};if imported(true)!="outer" || leaf.Seen!=7 {panic("imported inner ownership")};if len(leaf.Trace)!=2 || leaf.Trace[0]!=20 || leaf.Trace[1]!=20 {panic("imported cleanup")};println("switch-return-ok")}
`,
	}
	t.Run("native-leaf", func(t *testing.T) {
		testSwitchReturnFiles(t, files, "example.com/paramnesteddefer/...=", nil, []string{"imported"})
	})
	t.Run("default-leaf", func(t *testing.T) { testSwitchReturnFiles(t, files, "", nil, nil) })
}
