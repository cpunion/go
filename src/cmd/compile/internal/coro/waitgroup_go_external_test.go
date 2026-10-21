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

func TestWaitGroupGo(t *testing.T) {
	const source = `package main
import (
 "runtime"
 "runtime/debug"
 "sync"
 "sync/atomic"
)
var group sync.WaitGroup
var ready chan int
var gate chan struct{}
var serial atomic.Int32
var values [128]int
func first() {
 n:=int(serial.Add(1))-1
 p:=new(int);*p=n+1
 ready<-n
 <-gate
 runtime.Gosched()
 values[n]=*p
}
func second() {first()}
func spawn() {
 for i:=range 128 {
  f:=first
  if i%2!=0 {f=second}
  group.Go(f)
 }
}
func plain() {values[0]=99}
func yielding() {runtime.Gosched();values[0]=100}
func mixed(which bool) {f:=plain;if which {f=yielding};group.Go(f)}
func choose(which bool) func() {runtime.Gosched();if which {return yielding};return plain}
func returned(which bool) {f:=choose(which);alias:=f;group.Go(alias)}
func exit() {defer func(){values[1]=7}();runtime.Gosched();runtime.Goexit();values[1]=999}
func exiting() {group.Go(exit)}
var nestedCount atomic.Int32
func nested() {if nestedCount.Add(1)<64 {group.Go(nested)};runtime.Gosched()}
func nestedSpawn() {group.Go(nested)}
func receiverPanic() *sync.WaitGroup {runtime.Gosched();panic("receiver")}
func argumentPanic() (v any) {defer func(){v=recover()}();receiverPanic().Go(plain);return}
func nilReceiver() (v any) {defer func(){v=recover()}();var wg *sync.WaitGroup;wg.Go(plain);return}
var broken sync.WaitGroup
func negative() {defer func(){recover()}();broken.Add(-2)}
func addPanic() (v any) {defer func(){v=recover()}();broken.Go(plain);return}
var receiverCount int
func receiver() *sync.WaitGroup {runtime.Gosched();receiverCount++;return &group}
func receiverOnce() {f:=yielding;receiver().Go(f)}
func nilDone() {close(exited)}
var exited chan struct{}
func receiverExit() *sync.WaitGroup {runtime.Gosched();runtime.Goexit();return &group}
func argumentExit() {defer nilDone();receiverExit().Go(plain)}
var global=yielding
func parameter(f func()) {group.Go(f)}
func globalCallback() {group.Go(global)}
func closureCallback() {x:=101;group.Go(func(){runtime.Gosched();values[0]=x})}
func missing() {runtime.Gosched();goto end;end:values[0]=102}
func missingFactory(which bool) {f:=yielding;if which {f=missing};group.Go(f)}
func plainLoop() {for i:=range 4 {values[0]=i}}
func plainLoopSpawn() {f:=plainLoop;group.Go(f)}
func main() {
 native:=false
 if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 ready,gate=make(chan int,128),make(chan struct{})
 baseline:=runtime.NumGoroutine()
 finished:=make(chan struct{})
 go func(){spawn();close(finished)}()
 <-finished
 var seen [128]bool
 for range 128 {n:=<-ready;if n<0 || n>=128 || seen[n] {panic("callback identity")};seen[n]=true}
 if native && runtime.NumGoroutine()>baseline+16 {panic("WaitGroup.Go retained ordinary goroutines")}
 runtime.GC();close(gate);group.Wait()
 for i,n:=range values {if n!=i+1 {panic("callback completion or GC")}}
 mixed(false);group.Wait();if values[0]!=99 {panic("plain callback")}
 mixed(true);group.Wait();if values[0]!=100 {panic("mixed ABI")}
 returned(false);group.Wait();if values[0]!=99 {panic("returned plain")}
 returned(true);group.Wait();if values[0]!=100 {panic("returned yielding")}
 exiting();group.Wait();if values[1]!=7 {panic("Goexit Done")}
 nestedSpawn();group.Wait();if nestedCount.Load()!=64 {panic("nested Go")}
 if argumentPanic()!="receiver" || nilReceiver()==nil {panic("parent Add or argument panic")}
 negative();values[0]=1234
 if addPanic()==nil || values[0]!=1234 {panic("Add failure spawned callback")}
 group.Wait()
 receiverOnce();group.Wait();if receiverCount!=1 || values[0]!=100 {panic("receiver evaluated once")}
 exited=make(chan struct{});go argumentExit();<-exited;group.Wait()
 parameter(plain);group.Wait();if values[0]!=99 {panic("parameter fallback")}
 globalCallback();group.Wait();if values[0]!=100 {panic("global fallback")}
 closureCallback();group.Wait();if values[0]!=101 {panic("closure fallback")}
 missingFactory(true);group.Wait();if values[0]!=102 {panic("missing factory fallback")}
 missingFactory(false);group.Wait();if values[0]!=100 {panic("mixed missing fallback")}
 plainLoopSpawn();group.Wait();if values[0]!=3 {panic("all-plain missing factory fallback")}
 group.Add(1)
 go func(){runtime.Gosched();values[2]=9;group.Done()}()
 group.Wait();if values[2]!=9 {panic("ordinary interoperability")}
 println("switch-return-ok")
}
`
	fallback := []switchFallback{
		{"parameter", "unsupported coroutine dependency sync.(*WaitGroup).Go"},
		{"globalCallback", "unsupported coroutine dependency sync.(*WaitGroup).Go"},
		{"closureCallback", "unsupported coroutine dependency sync.(*WaitGroup).Go"},
		{"missingFactory", "unsupported coroutine dependency main.missingFactory.wggo"},
		{"plainLoopSpawn", "unsupported coroutine dependency main.plainLoopSpawn.wggo"},
	}
	lowered := []string{"spawn", "mixed", "returned", "exiting", "nestedSpawn", "receiverOnce", "argumentPanic", "nilReceiver", "addPanic", "argumentExit"}
	t.Run("default-sync", func(t *testing.T) { testSwitchReturnProgram(t, source, fallback, lowered) })
	t.Run("native-sync", func(t *testing.T) {
		testSwitchReturnFiles(t, map[string]string{"main.go": source}, "", fallback, lowered,
			"-gcflags=sync=-l -d=coro=4", "-gcflags=internal/sync=-l -d=coro=4")
	})
}

func TestWaitGroupGoFatal(t *testing.T) {
	const source = `package main
import (
 "context"
 "os"
 "os/exec"
 "runtime"
 "strings"
 "sync"
 "time"
)
var group sync.WaitGroup
func normal() {runtime.Gosched()}
func failing() {defer func(){println("callback-cleanup")}();runtime.Gosched();panic("waitgroup-child-panic")}
func panicNil() {runtime.Gosched();panic(nil)}
func missingPanic() {runtime.Gosched();goto end;end:panic("waitgroup-fallback-panic")}
func missingNil() {runtime.Gosched();goto end;end:panic(nil)}
func invokeFallback(mode string) {f:=missingPanic;if mode=="fallback-legacy" {f=missingNil};group.Go(f)}
func invoke(mode string) {
 f:=normal
 switch mode {case "nil":f=nil;case "panic":f=failing;case "panicnil":f=panicNil}
 group.Go(f)
}
func main() {
 if len(os.Args)>1 {
  defer func(){if recover()!=nil {println("parent-recovered-child")}}()
  if strings.HasPrefix(os.Args[1],"fallback-") {invokeFallback(os.Args[1])} else {invoke(os.Args[1])}
  group.Wait()
  // Expose a wrongly unblocked Wait even if fatal panic propagation loses
  // the first scheduling race. The source wrapper does not call Done.
  time.Sleep(20*time.Millisecond)
  println("wait-returned")
  os.Exit(0)
 }
 exe,err:=os.Executable();if err!=nil {panic(err)}
 for _,mode:=range []string{"nil","panic","panicnil","legacy-nil","fallback-panic","fallback-legacy"} {
  arg:=mode
  if mode=="legacy-nil" {arg="panicnil"}
  ctx,cancel:=context.WithTimeout(context.Background(),2*time.Second)
  cmd:=exec.CommandContext(ctx,exe,arg)
  setting:="panicnil=0";if mode=="legacy-nil" || mode=="fallback-legacy" {setting="panicnil=1"}
  cmd.Env=append(os.Environ(),"GODEBUG="+setting)
  data,err:=cmd.CombinedOutput();expired:=ctx.Err()!=nil;cancel()
  out:=string(data)
  if mode=="legacy-nil" || mode=="fallback-legacy" {
   if expired || err!=nil || strings.TrimSpace(out)!="wait-returned" {panic("legacy panicnil source policy: "+out)}
   continue
  }
  want:="invalid memory address or nil pointer dereference"
  if mode=="panic" {want="waitgroup-child-panic"}
  if mode=="panicnil" {want="panic called with nil argument"}
  if mode=="fallback-panic" {want="waitgroup-fallback-panic"}
  if expired || err==nil || !strings.Contains(out,want) || strings.Contains(out,"parent-recovered-child") || strings.Contains(out,"wait-returned") || strings.Contains(out,"go of nil func value") {panic("child ownership or premature Done: "+mode+": "+out)}
  if mode=="panic" && strings.Count(out,"callback-cleanup")!=1 {panic("callback cleanup: "+out)}
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"invoke"})
}

func TestWaitGroupGoImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/waitgroupgo\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
var Value int
func First() {runtime.Gosched();Value=1}
func Second() {defer func(){Value++}();runtime.Gosched();Value=2}
func Plain() {Value=4}
func Demand() {go Plain()}
func Unavailable() {Value=5}
`,
		"creator/creator.go": `package creator
import (
 "sync"
 "example.com/waitgroupgo/leaf"
)
func Spawn(wg *sync.WaitGroup, which int) {
 f:=leaf.First
 if which==1 {f=leaf.Second}
 if which==2 {f=leaf.Plain}
 wg.Go(f)
}

func Missing(wg *sync.WaitGroup) {wg.Go(leaf.Unavailable)}
`,
		"main.go": `package main
import (
 "sync"
 "example.com/waitgroupgo/creator"
 "example.com/waitgroupgo/leaf"
)
func run(wg *sync.WaitGroup,n int) {creator.Spawn(wg,n)}
func main() {
 var wg sync.WaitGroup
 for n,want:=range []int{1,3,4} {run(&wg,n);wg.Wait();if leaf.Value!=want {panic("imported callback factory")}}
 creator.Missing(&wg);wg.Wait();if leaf.Value!=5 {panic("imported missing factory fallback")}
 println("switch-return-ok")
}
`,
	}
	testSwitchReturnFiles(t, files, "example.com/waitgroupgo/...=", nil, []string{"run"})
}

func TestWaitGroupGoContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package wgcontext
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
func spawnSlow(wg *sync.WaitGroup) {wg.Go(slow)}
func spawnFast(wg *sync.WaitGroup) {wg.Go(fast)}
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
		"go.mod":          "module example.com/wgcontext\n\ngo 1.28\n",
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
			args = append(args, "-gcflags=example.com/wgcontext="+flags)
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
				present := strings.Contains(string(data), "example.com/wgcontext."+name+".coro")
				if present != (mode == "inline" || mode == "noinline") {
					t.Fatalf("%s: native factory present=%v", name, present)
				}
			}
		})
	}
}
