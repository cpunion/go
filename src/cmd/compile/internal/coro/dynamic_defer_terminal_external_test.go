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

func TestDynamicDeferTerminal(t *testing.T) {
	const source = `package main
import "runtime"

var observed any
var trace []int
func recoverA() {observed=recover();trace=append(trace,1)}
func recoverB() {observed=recover();trace=append(trace,2)}
func direct(which bool) {
 f:=recoverA;if which {f=recoverB}
 defer f()
 f=recoverB
 runtime.Gosched()
 panic("owned")
}
func plain(n int) int {trace=append(trace,n);return n}
func panicA(n int) int {trace=append(trace,n);panic(n)}
func panicB(n int) int {trace=append(trace,100+n);panic(100+n)}
func maybe(n int) int {trace=append(trace,n);if n<0 {panic(n)};return n}
func conditional(which bool,n int) (result any) {
 defer func(){result=recover()}()
 f:=plain;if which {f=maybe}
 defer f(n)
 runtime.Gosched()
 return
}
func panicArgument() int {runtime.Gosched();panic("argument")}
func argumentPanic(nilTarget bool) (result any) {
 defer func(){result=recover()}()
 f:=panicA;if nilTarget {f=nil}
 defer f(panicArgument())
 return
}
func replacement(which bool, nilTarget bool) (result any) {
 defer func(){result=recover()}()
 f:=panicA;if which {f=panicB};if nilTarget {f=nil}
 defer f(7)
 f=panicB
 runtime.Gosched()
 panic("original")
}
func repeated() (result any) {
 defer func(){result=recover()}()
 for i:=0;i<4;i++ {f:=plain;if i%2!=0 {f=panicA};defer f(i)}
 runtime.Gosched()
 return
}
func recoverReplace() {observed=recover();panic("replacement")}
func recoverPlain() {observed=recover()}
func recoverAndReplace(which bool) (result any) {
 defer func(){result=recover()}()
 f:=recoverPlain;if which {f=recoverReplace}
 defer f()
 runtime.Gosched()
 panic("original")
}
func exitA() {trace=append(trace,8);runtime.Goexit()}
func exitB() {trace=append(trace,9);runtime.Goexit()}
func exiting(which bool, done chan struct{}) {
 defer func(){close(done)}()
 defer func(){observed=recover()}()
 f:=exitA;if which {f=exitB}
 defer f()
 runtime.Gosched()
 panic("suppressed")
}
func recoveredExit(which bool, done chan struct{}) {
 defer func(){close(done)}()
 f:=recoverA;if which {f=recoverB}
 defer f()
 runtime.Gosched()
 runtime.Goexit()
 trace=append(trace,1000)
}
func failAndExit() {defer exitA();panic("inner")}
func nested(which bool, done chan struct{}) {
 defer func(){close(done)}()
 defer func(){observed=recover()}()
 f:=exitB;if which {f=failAndExit}
 defer f()
 runtime.Gosched()
 panic("outer")
}
func helperRecover() {observed=recover()}
func indirectRecover() {helperRecover()}
func indirect(which bool) (result any) {
 defer func(){result=recover()}()
 f:=recoverA;if which {f=indirectRecover}
 defer f()
 runtime.Gosched()
 panic("indirect")
}
func missing() {goto end;end:panic("missing")}
func missingFactory(which bool) (result any) {
 defer func(){result=recover()}()
 f:=recoverA;if which {f=missing}
 defer f()
 runtime.Gosched()
 panic("original")
}
func nestA(n int) {f:=recoverA;if n!=0 {f=recoverB};defer f();if n!=0 {panic("inner")}}
func nestB(n int) {f:=recoverB;if n!=0 {f=recoverA};defer f();if n!=0 {panic("inner")}}
func nestedRecover(which bool,n int) (result any) {
 defer func(){result=recover()}()
 f:=nestA;if which {f=nestB}
 defer f(n)
 runtime.Gosched()
 panic("outer")
}
func equal(want ...int) {if len(trace)!=len(want) {panic("trace length")};for i,v:=range want {if trace[i]!=v {panic("trace order or replay")}};trace=nil}
func main() {
 direct(false);if observed!="owned" {panic("direct task recovery")};equal(1)
 direct(true);if observed!="owned" {panic("other task recovery")};equal(2)
 if conditional(false,5)!=nil || conditional(true,6)!=nil || conditional(true,-7)!=-7 {panic("discarded factory return")};equal(5,6,-7)
 if argumentPanic(false)!="argument" || argumentPanic(true)!="argument" {panic("registration panic ownership")};equal()
 if replacement(false,false)!=7 {panic("first replacement")};equal(7)
 if replacement(true,false)!=107 {panic("second replacement")};equal(107)
 if v:=replacement(false,true);v==nil || v=="original" {panic("delayed nil replacement")};equal()
 if repeated()!=1 {panic("repeated replacement panic")};equal(3,2,1,0)
 if recoverAndReplace(false)!=nil || observed!="original" {panic("plain recover target")}
 if recoverAndReplace(true)!="replacement" || observed!="original" {panic("factory recover and replace")}
 done:=make(chan struct{});go exiting(false,done);<-done
 if observed!=nil {panic("Goexit did not suppress panic")};equal(8)
 done=make(chan struct{});go exiting(true,done);<-done
 if observed!=nil {panic("other Goexit did not suppress panic")};equal(9)
 done=make(chan struct{});go recoveredExit(false,done);<-done
 if observed!=nil {panic("recover returned Goexit")};equal(1)
 done=make(chan struct{});go recoveredExit(true,done);<-done
 if observed!=nil {panic("other recover returned Goexit")};equal(2)
 done=make(chan struct{});go nested(true,done);<-done
 if observed!=nil {panic("nested Goexit ownership")};equal(8)
 observed=nil
 if indirect(true)!="indirect" || observed!=nil {panic("indirect recover became direct")}
 if missingFactory(true)!="missing" {panic("missing factory fallback")}
 if nestedRecover(false,0)!="outer" || observed!=nil {panic("nested defer recovered outer panic")};equal(1)
 if nestedRecover(false,1)!="outer" || observed!="inner" {panic("nested own panic ownership")};equal(2)
 if nestedRecover(true,0)!="outer" || observed!=nil {panic("other nested defer recovered outer panic")};equal(2)
 if nestedRecover(true,1)!="outer" || observed!="inner" {panic("other nested own panic ownership")};equal(1)
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{
			{"indirect", "dynamic defer target main.indirectRecover has unsupported terminal control recover"},
			{"missingFactory", "unsupported coroutine dependency main.missing"},
		},
		[]string{"direct", "conditional", "argumentPanic", "replacement", "repeated", "recoverAndReplace", "exiting", "recoveredExit", "nested", "nestedRecover", "nestA", "nestB"})
}

func TestDynamicDeferTerminalImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/terminaldefer\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
var Observed any
func RecoverA() {Observed=recover()}
func RecoverB() {Observed=recover()}
func First(n int) int {if n<0 {panic(n)};return n}
func Second(n int) int {if n<0 {panic(n-1)};return n}
func ExitA() {runtime.Goexit()}
func ExitB() {runtime.Goexit()}
`,
		"main.go": `package main
import (
 "runtime"
 "example.com/terminaldefer/leaf"
)
func recovered(which bool) {f:=leaf.RecoverA;if which {f=leaf.RecoverB};defer f();runtime.Gosched();panic("imported")}
func replacement(which bool,n int) (result any) {defer func(){result=recover()}();f:=leaf.First;if which {f=leaf.Second};defer f(n);runtime.Gosched();return}
func exiting(which bool,done chan struct{}) {defer func(){close(done)}();f:=leaf.ExitA;if which {f=leaf.ExitB};defer f();runtime.Gosched()}
func main() {
 recovered(false);if leaf.Observed!="imported" {panic("imported direct recovery")}
 recovered(true);if leaf.Observed!="imported" {panic("other imported direct recovery")}
 if replacement(false,5)!=nil || replacement(false,-7)!=-7 || replacement(true,-8)!=-9 {panic("imported factory")}
 done:=make(chan struct{});go exiting(false,done);<-done
 done=make(chan struct{});go exiting(true,done);<-done
 println("switch-return-ok")
}

`,
	}
	t.Run("native-leaf", func(t *testing.T) {
		testSwitchReturnFiles(t, files, "example.com/terminaldefer/...=", nil, []string{"recovered", "replacement", "exiting"})
	})
	t.Run("default-leaf", func(t *testing.T) {
		testSwitchReturnFiles(t, files, "", []switchFallback{
			{"recovered", "dynamic defer target example.com/terminaldefer/leaf.RecoverA has unsupported terminal control recover"},
			{"replacement", "dynamic defer target example.com/terminaldefer/leaf.First has unsupported terminal control panic"},
			{"exiting", "dynamic defer target example.com/terminaldefer/leaf.ExitA has unsupported terminal control goexit"},
		}, nil)
	})
}

func TestDynamicDeferTerminalContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package defercontext
import (
 "runtime"
 "testing"
 "testing/synctest"
 "time"
)
func first(out *any) {*out=recover()}
func second(out *any) {*out=recover()}
func deferred(which bool, delay time.Duration) (out any) {
 f:=first;if which {f=second}
 defer f(&out)
 f=second
 runtime.Gosched()
 time.Sleep(delay)
 panic("context")
}
func TestBubble(t *testing.T) {
 for range 1 {
  synctest.Test(t,func(t *testing.T){
   start:=time.Now()
   if deferred(false,time.Hour)!="context" || deferred(true,time.Hour)!="context" {t.Fatal("ordinary bubble defer/recover")}
   if time.Since(start)!=2*time.Hour {t.Fatal("ordinary bubble clock")}
   synctest.Wait()
  })
 }
}
func TestThread(t *testing.T) {
 for range 1 {
  runtime.LockOSThread()
  defer runtime.UnlockOSThread()
  if deferred(false,0)!="context" || deferred(true,0)!="context" {t.Fatal("ordinary thread-bound defer/recover")}
 }
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{
		"go.mod": "module example.com/defercontext\n\ngo 1.28\n", "context_test.go": source,
	} {
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
			args = append(args, "-gcflags=example.com/defercontext="+flags)
			build := testenv.Command(t, testenv.GoToolPath(t), args...)
			build.Dir = dir
			build.Env = append(build.Environ(), "GOEXPERIMENT="+experiment, "GOWORK=off")
			if data, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, data)
			}
			if data, err := testenv.Command(t, exe, "-test.timeout=10s").CombinedOutput(); err != nil {
				t.Fatalf("contexts: %v\n%s", err, data)
			}
			data, err := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe).CombinedOutput()
			if err != nil {
				t.Fatal(err)
			}
			present := strings.Contains(string(data), "example.com/defercontext.deferred.coro")
			if present != (mode == "inline" || mode == "noinline") {
				t.Fatalf("native factory present=%v", present)
			}
		})
	}
}
