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

func TestClosedParameterDefer(t *testing.T) {
	const source = `package main
import ("runtime";"runtime/debug")
type box struct {n int}
var trace []int
func first(p *box,n int) int {trace=append(trace,p.n+n);return n}
func second(p *box,n int) int {trace=append(trace,p.n+n+100);return n}
//go:noinline
func apply(f func(*box,int)int,p *box,n int) int {return f(p,n)}
var arguments int
func argument() int {arguments++;return arguments}
func repeated() {
 for i:=range 4 {f:=first;if i%2!=0 {f=second};defer apply(f,&box{i},argument());f=nil}
 runtime.Gosched()
}
func sum(xs ...int) int {if len(xs)==0 {panic("empty")};n:=0;for i:=range len(xs) {n+=xs[i]};trace=append(trace,n);return n}
//go:noinline
func variable(f func(...int)int,xs ...int) int {return f(xs...)}
func variadic() {defer variable(sum,2,3);runtime.Gosched()}
func increment(p *int) {(*p)++}
//go:noinline
func mutate(f func(*int),p *int) {f(p)}
func named() (n int) {defer mutate(increment,&n);runtime.Gosched();return 9}
func equal(want ...int) {if len(trace)!=len(want) {panic("trace length")};for i:=range len(want) {if trace[i]!=want[i] {panic("snapshot or LIFO order")}};trace=nil}
func mark(p *box,n int) int {p.n+=n;return p.n}
func parked(p *box,ready chan int,gate,done chan struct{}) {defer func(){close(done)}();defer apply(mark,p,7);ready<-1;<-gate}
func start(p *box,ready chan int,gate,done chan struct{}) {go parked(p,ready,gate,done)}
func main(){
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 baseline:=runtime.NumGoroutine();ready,gate:=make(chan int,128),make(chan struct{});ps:=make([]*box,128);ds:=make([]chan struct{},128)
 for i:=range 128 {p,done:=&box{i},make(chan struct{});ps[i],ds[i]=p,done;start(p,ready,gate,done)}
 for range 128 {<-ready};if native && runtime.NumGoroutine()>baseline+12 {panic("parameter defer ordinary goroutines")};runtime.GC();close(gate)
 for i:=range 128 {<-ds[i];if ps[i].n!=i+7 {panic("deferred pointer GC ownership")}}
 repeated();if arguments!=4 {panic("argument evaluation count")};equal(107,5,103,1);variadic();equal(5);if named()!=10 {panic("named result defer")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, nil, []string{"repeated", "variadic", "named", "parked", "start"})
}

func TestClosedParameterDeferTerminal(t *testing.T) {
	const source = `package main
import "runtime"
var trace []int
var observed any
func plain(n int) int {trace=append(trace,n);return n}
func failing(n int) int {trace=append(trace,n);panic(n)}
func exiting(n int) int {trace=append(trace,n);runtime.Goexit();return n}
func recovering(n int) int {observed=recover();trace=append(trace,n);return n}
//go:noinline
func apply(f func(int)int,n int) int {return f(n)}
func replacement(which int) (result any) {
 defer func(){result=recover()}()
 f:=plain;switch which {case 1:f=failing;case 2:f=nil}
 defer apply(f,7)
 f=nil
 runtime.Gosched()
 panic("original")
}
//go:noinline
func applyRecover(f func(int)int,n int) {observed=recover();f(n)}
func direct() {defer applyRecover(plain,6);runtime.Gosched();panic("direct")}
func nilPlain(which bool) (result any) {defer func(){result=recover()}();f:=plain;if which {f=nil};defer apply(f,4);runtime.Gosched();return}
func indirect() (result any) {
 defer func(){result=recover()}()
 defer apply(recovering,8)
 runtime.Gosched()
 panic("owned")
}
func exit(done chan struct{}) {
 defer func(){close(done)}()
 defer func(){observed=recover()}()
 defer apply(exiting,9)
 runtime.Gosched()
 panic("suppressed")
}
var side int
func bad() int {side++;panic("argument")}
func argumentPanic() (result any) {defer func(){result=recover()}();defer apply(failing,bad());return}
func badExit() int {side++;runtime.Goexit();return 0}
func argumentExit(done chan struct{}) {defer func(){close(done)}();defer apply(failing,badExit());panic("continued")}
func repeated() (result any) {defer func(){result=recover()}();for i:=range 4 {f:=plain;if i%2!=0 {f=failing};defer apply(f,i)};runtime.Gosched();return}
func equal(want ...int) {if len(trace)!=len(want) {panic("trace length")};for i:=range len(want) {if trace[i]!=want[i] {panic("terminal trace order")}};trace=nil}
func main(){
 if replacement(0)!="original" {panic("plain deferred callback")};equal(7)
 if replacement(1)!=7 {panic("replacement panic")};equal(7)
 if _,ok:=replacement(2).(runtime.Error);!ok {panic("delayed nil callback")};equal()
 if nilPlain(false)!=nil {panic("plain callback result")};equal(4)
 if _,ok:=nilPlain(true).(runtime.Error);!ok {panic("nil all-plain callback")};equal()
 direct();if observed!="direct" {panic("direct helper recovery")};equal(6)
 if indirect()!="owned" || observed!=nil {panic("indirect recover stole panic")};equal(8)
 done:=make(chan struct{});go exit(done);<-done;if observed!=nil {panic("Goexit did not suppress panic")};equal(9)
 if argumentPanic()!="argument" || side!=1 {panic("registration panic ownership")};equal()
 done=make(chan struct{});go argumentExit(done);<-done;if side!=2 {panic("registration Goexit ownership")};equal()
 if repeated()!=1 {panic("repeated callback panic")};equal(3,2,1,0)
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, []switchFallback{{"indirect", "defer target has execution constraints task-context"}}, []string{"replacement", "nilPlain", "direct", "exit", "argumentPanic", "argumentExit", "repeated"})
}

func TestClosedParameterDeferBoundaries(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/paramdefer\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
func Plain(p *int) {(*p)+=10;if *p<0 {panic("negative")}}
//go:noinline
func Apply(f func(*int),p *int) {f(p)}
`,
		"main.go": `package main
import (
 "runtime"
 "example.com/paramdefer/leaf"
)
func first(p *int) {(*p)++}
func slow(p *int) {runtime.Gosched();(*p)+=100}
//go:noinline
func apply(f func(*int),p *int) {f(p)}
func imported(p *int) {f:=first;if *p!=0 {f=leaf.Plain};defer apply(f,p);runtime.Gosched()}
func suspending(p *int) {defer apply(slow,p);runtime.Gosched()}
func open(f func(*int),p *int) {defer apply(f,p);runtime.Gosched()}
var global=first
func globalCall(p *int) {defer apply(global,p);runtime.Gosched()}
func importedCallee(p *int) {defer leaf.Apply(first,p);runtime.Gosched()}
func rejected(p *int) {defer apply(first,p);goto end;end:runtime.Gosched()}
//go:noinline
func inspection(f func()) {f()}
func inspect() {pc,_,_,ok:=runtime.Caller(1);if !ok || runtime.FuncForPC(pc).Name()!="main.inspection" {panic("ordinary source frame changed")}}
func rejectedInspection() {defer inspection(inspect);goto end;end:runtime.Gosched()}
func main(){
 n:=0;imported(&n);imported(&n);if n!=11 {panic("imported callback")}
 suspending(&n);open(first,&n);globalCall(&n);importedCallee(&n);rejected(&n);if n!=115 {panic("ordinary fallback")}
 rejectedInspection();println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/paramdefer/...=",
		[]switchFallback{{"suspending", "suspending defer"}, {"rejected", "control flow goto"}, {"rejectedInspection", "control flow goto"}},
		[]string{"imported"})
}

func TestClosedParameterDeferContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package paramdefercontext
import (
 "runtime"
 "testing"
 "testing/synctest"
 "time"
)
var value int
func plain() {value++}
//go:noinline
func apply(f func()) {f()}
func deferredSlow() {defer apply(plain);time.Sleep(time.Hour)}
func deferredFast() {defer apply(plain);runtime.Gosched()}
func TestBubble(t *testing.T) {
 for range 1 {
  synctest.Test(t,func(t *testing.T){
   start:=time.Now()
   value=0
   deferredSlow()
   if value!=1 || time.Since(start)!=time.Hour {t.Fatal("callback lost ordinary synctest accounting")}
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
  if value!=1 {t.Fatal("thread-bound callback fallback")}
 }
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod":          "module example.com/paramdefercontext\n\ngo 1.28\n",
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
			args = append(args, "-gcflags=example.com/paramdefercontext="+flags)
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
					if len(fields) == 3 && (fields[1] == "T" || fields[1] == "t") && fields[2] == "example.com/paramdefercontext."+name+".coro" {
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
