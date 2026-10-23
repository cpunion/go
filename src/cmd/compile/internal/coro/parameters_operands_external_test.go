// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/testenv"
	"os"
	"path/filepath"
	"testing"
)

func TestClosedParameterOperands(t *testing.T) {
	const source = `package main
import "runtime"
type state struct {trace []int;seen any}
func first(p *state,n int) {p.trace=append(p.trace,n)}
func second(p *state,n int) {p.trace=append(p.trace,n+10)}
func boom(p *state,n int) {p.trace=append(p.trace,n);panic(n)}
func catch(p *state,n int) {p.seen=recover();p.trace=append(p.trace,n)}
func exit(p *state,n int) {p.trace=append(p.trace,n);runtime.Goexit()}
var arguments int
func argument() int {arguments++;return arguments}
//go:noinline
func apply(f func(*state,int),p *state) {for range 3 {defer f(p,argument())};f(p,4)}
func repeated(p *state,which bool) {f:=first;if which {f=second};apply(f,p);f=nil;runtime.Gosched()}
//go:noinline
func only(f func(*state,int),p *state,n int) {defer f(p,n)}
func unwind(p *state,which int) (seen any) {
 defer func(){seen=recover()}()
 f:=first;switch which {case 1:f=boom;case 2:f=catch;case 3:f=nil}
 defer only(f,p,7)
 runtime.Gosched()
 panic("outer")
}
func exiting(p *state,done chan struct{}) {defer func(){close(done)}();defer only(exit,p,8);runtime.Gosched();panic("outer")}
func bad() int {arguments++;panic("argument")}
//go:noinline
func registration(f func(*state,int),p *state) {defer first(p,9);defer f(p,bad())}
func failed(p *state) (seen any) {defer func(){seen=recover()}();registration(boom,p);runtime.Gosched();return}
func exitArgument() int {runtime.Goexit();return 0}
//go:noinline
func registrationExit(f func(*state,int),p *state,done chan struct{}) {defer func(){close(done)}();defer first(p,10);defer f(p,exitArgument());panic("continued after Goexit")}
func startExit(p *state,done chan struct{}) {go registrationExit(boom,p,done)}
func equal(p *state,want ...int) {if len(p.trace)!=len(want) {panic("snapshot length")};for i,v:=range want {if p.trace[i]!=v {panic("snapshot order")}}}
func main(){
 p:=new(state);repeated(p,false);equal(p,4,3,2,1)
 p=new(state);repeated(p,true);equal(p,14,16,15,14);if arguments!=6 {panic("argument evaluation count")}
 for which:=range 4 {p=new(state);seen:=unwind(p,which);switch which {
 case 0:if seen!="outer" {panic("outer lost")};equal(p,7)
 case 1:if seen!=7 {panic("replacement panic")};equal(p,7)
 case 2:if seen!="outer" || p.seen!=nil {panic("indirect recover stole outer panic")};equal(p,7)
 case 3:if _,ok:=seen.(runtime.Error);!ok {panic("delayed nil callback")}
 }}
 p=new(state);done:=make(chan struct{});go exiting(p,done);<-done;equal(p,8)
 p=new(state);if failed(p)!="argument" || arguments!=7 {panic("registration panic")};equal(p,9)
 p=new(state);done=make(chan struct{});startExit(p,done);<-done;equal(p,10)
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"repeated", "unwind", "exiting", "failed", "startExit"})
}

func TestClosedParameterOperandsSpawn(t *testing.T) {
	const source = `package main
import ("runtime";"runtime/debug")
type box struct {n int}
func first(p *box,n int,ready chan int,gate chan struct{},done chan int) {ready<-n;<-gate;runtime.Gosched();done<-p.n+n}
func second(p *box,n int,ready chan int,gate chan struct{},done chan int) {ready<-n;<-gate;runtime.Gosched();done<-p.n+n+1}
//go:noinline
func apply(f func(*box,int,chan int,chan struct{},chan int),p *box,n int,ready chan int,gate chan struct{},done chan int) {go f(p,n,ready,gate,done)}
//go:noinline
func forward(f func(*box,int,chan int,chan struct{},chan int),p *box,n int,ready chan int,gate chan struct{},done chan int) {go apply(f,p,n,ready,gate,done)}
var arguments int
func argument() int {arguments++;return arguments}
func spawn(ready chan int,gate chan struct{},done chan int) {
 for i:=range 128 {f:=first;if i%2!=0 {f=second};forward(f,&box{i},argument(),ready,gate,done);f=nil}
}
func main(){
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 baseline:=runtime.NumGoroutine();spawn(ready,gate,done)
 total:=0;for range 128 {total+=<-ready};if total!=8256 || arguments!=128 {panic("nested go operands")}
 if native && runtime.NumGoroutine()>baseline+12 {panic("ordinary callback tasks")}
 runtime.GC();close(gate);total=0;for range 128 {total+=<-done};if total!=16448 {panic("callback GC or lifetime")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"spawn"})
}

func TestClosedParameterOperandsBoundaries(t *testing.T) {
	const source = `package main
import "runtime"
var trace int
func first() {trace=trace*10+1}
func second() {trace=trace*10+2}
//go:noinline
func write(f func()) {defer f();f=second;f()}
//go:noinline
func address(f func()) {p:=&f;defer f();*p=second;f()}
//go:noinline
func capture(f func()) {defer func(){f()}();f=second}
//go:noinline
func ordinary(f func()) {defer f();f()}
func inspect() {pc,_,_,ok:=runtime.Caller(1);if !ok || runtime.FuncForPC(pc).Name()!="main.ordinary" {panic("ordinary helper not restored")}}
func rejected() {ordinary(inspect);goto end;end:runtime.Gosched()}
func open(f func()) {ordinary(f);runtime.Gosched()}
var global=first
func dynamic() {ordinary(global);runtime.Gosched()}
//go:noinline
func spawn(f func()) {go f()}
func deferred(done chan struct{}) {defer func(){close(done)}();defer spawn(runtime.Gosched);runtime.Gosched()}
func main(){
 write(first);if trace!=21 {panic("mutable callback snapshot")};trace=0
 address(first);if trace!=21 {panic("addressed callback snapshot")};trace=0
 capture(first);if trace!=2 {panic("source closure mutation")};trace=0
 rejected();open(first);dynamic();if trace!=1111 {panic("open callback fallback")};trace=0
 done:=make(chan struct{});deferred(done);<-done
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, []switchFallback{{"rejected", "control flow goto"}, {"deferred", "defer target has execution constraints task-context"}}, nil)
}

func TestClosedParameterOperandsImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/paramoperands\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
var Value int
func Record(n int) {if n<0 {panic(n)};Value=Value*10+n}
func Child(n int,done chan int) {runtime.Gosched();done<-n+10}
`,
		"main.go": `package main
import ("runtime";"example.com/paramoperands/leaf")
//go:noinline
func deferApply(f func(int),n int) {defer f(n)}
//go:noinline
func goApply(f func(int,chan int),n int,done chan int) {go f(n,done)}
func deferred() {defer deferApply(leaf.Record,3);runtime.Gosched()}
func creating(done chan int) {goApply(leaf.Child,7,done);runtime.Gosched()}
func main(){deferred();if leaf.Value!=3 {panic("imported deferred operands")};done:=make(chan int);creating(done);if <-done!=17 {panic("imported go operands")};println("switch-return-ok")}
`,
	}
	t.Run("native-leaf", func(t *testing.T) {
		testSwitchReturnFiles(t, files, "example.com/paramoperands/...=", nil, []string{"deferred", "creating"})
	})
	t.Run("default-leaf", func(t *testing.T) { testSwitchReturnFiles(t, files, "", nil, nil) })
}

func TestClosedParameterOperandsContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	const source = `package paramoperandcontexts
import ("runtime";"testing";"testing/synctest";"time")
var value int
func plain() {value++}
//go:noinline
func apply(f func()) {defer f()}
func slow() {defer apply(plain);time.Sleep(time.Hour)}
//go:noinline
func spawn(f func(chan int),done chan int) {go f(done)}
func child(done chan int) {runtime.Gosched();done<-7}
func creating(done chan int) {spawn(child,done);runtime.Gosched()}
func TestBubble(t *testing.T) {for range 1 {synctest.Test(t,func(t *testing.T){start:=time.Now();value=0;slow();if value!=1 || time.Since(start)!=time.Hour {t.Fatal("snapshot callback bubble accounting")};done:=make(chan int);creating(done);if <-done!=7 {t.Fatal("callback child bubble")};synctest.Wait()})}}
func TestThread(t *testing.T) {for range 1 {runtime.LockOSThread();defer runtime.UnlockOSThread();value=0;apply(plain);if value!=1 {t.Fatal("thread-bound deferred snapshot")};done:=make(chan int);creating(done);if <-done!=7 {t.Fatal("thread-bound child")}}}
`
	dir := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module example.com/paramoperandcontexts\n\ngo 1.28\n", "context_test.go": source} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o666); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"inline", "noinline", "race", "nocoro"} {
		t.Run(mode, func(t *testing.T) {
			exe := filepath.Join(t.TempDir(), "contexts.exe")
			args := []string{"test", "-c", "-o", exe}
			experiment, flags := "coro", "-d=coro=4"
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
			args = append(args, "-gcflags=example.com/paramoperandcontexts="+flags)
			cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
			cmd.Dir, cmd.Env = dir, append(cmd.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			cmd = testenv.Command(t, exe, "-test.timeout=10s")
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("contexts: %v\n%s", err, data)
			}
		})
	}
}

func TestClosedParameterOperandsFatal(t *testing.T) {
	const source = `package main
import (
 "context"
 "os"
 "os/exec"
 "runtime"
 "strings"
 "time"
)
func first(n int) int {runtime.Gosched();return n}
func failing(n int) int {runtime.Gosched();panic("operand-child-panic")}
//go:noinline
func apply(f func(int)int,n int) {go f(n+argument())}
func argument() int {println("argument-once");return 1}
func spawn(which bool,nilTarget bool) {f:=first;if which {f=failing};if nilTarget {f=nil};apply(f,0)}
func main(){
 if len(os.Args)>1 {
  defer func(){if recover()!=nil {println("parent-recovered-child")}}()
  spawn(os.Args[1]=="panic",os.Args[1]=="nil")
  select {}
 }
 exe,err:=os.Executable();if err!=nil {panic(err)}
 for _,mode:=range []string{"nil","panic"} {
  ctx,cancel:=context.WithTimeout(context.Background(),2*time.Second)
  data,err:=exec.CommandContext(ctx,exe,mode).CombinedOutput()
  expired:=ctx.Err()!=nil;cancel();out:=string(data)
  want:="invalid memory address or nil pointer dereference";if mode=="panic" {want="operand-child-panic"}
  if expired || err==nil || !strings.Contains(out,want) || strings.Contains(out,"parent-recovered-child") {panic("child failure ownership: "+out)}
  count:=0;for _,line:=range strings.Split(out,"\n") {if line=="argument-once" {count++}};if count!=1 {panic("child argument evaluation: "+out)}
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"spawn"})
}
