// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/platform"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestArrayRange(t *testing.T) {
	const source = `package main
import ("runtime";"unsafe")
type numbers [3]int
type arrayPointer *numbers
var calls int
func snapshot(v [3]int) [3]int {calls++;return v}
func array(values *[3]int) int {
 sum:=0
 for i,v:=range *values {runtime.Gosched();sum+=10*i+v;values[1]=90;i=99;v=99}
 return sum
}
func pointer(values *[3]int) int {
 sum:=0;original:=values
 for i,v:=range values {runtime.Gosched();sum+=10*i+v;original[1]=90;values=nil;i=99;v=99}
 return sum
}
func named(values numbers,p arrayPointer) int {
 sum:=0;for _,v:=range values {runtime.Gosched();sum+=v}
 for _,v:=range p {runtime.Gosched();sum+=v};return sum
}
func evaluated(values [3]int) int {
 sum:=0;for _,v:=range snapshot(values) {runtime.Gosched();sum+=v};return sum
}
func keyOnly(p *[3]int) int {
 sum:=0
 for i:=range p {runtime.Gosched();sum+=i;i=99}
 for i:=range *p {runtime.Gosched();sum+=i}
 for i,_:=range *p {runtime.Gosched();sum+=i}
 for range *p {runtime.Gosched();sum++}
 for _,_=range *p {runtime.Gosched();sum++}
 return sum
}
func keyCalls(values [3]int) int {
 sum:=0;for i:=range snapshot(values) {runtime.Gosched();sum+=i}
 for range snapshot(values) {runtime.Gosched();sum++};return sum
}
func initialized(values [3]int) int {a:=snapshot(values);sum:=0;for _,v:=range a {runtime.Gosched();sum+=v};return sum}
func received(ch chan *[3]int) int {sum:=0;for i:=range <-ch {runtime.Gosched();sum+=i};return sum}
func zero(p *[0]int) int {n:=0;for i,v:=range p {runtime.Gosched();n+=i+v};return n}
func converted(values *[3]int) int {
 var i,v any;sum:=0;for i,v=range values {runtime.Gosched();sum+=i.(int)+v.(int)};return sum
}
func assignment(values [3]int) int {
 i:=2;out:=[]int{10,20,30,40};sum:=0
 for i,out[i]=range values {runtime.Gosched();sum+=i}
 if out[0]!=8||out[1]!=9||out[2]!=7||out[3]!=40 {panic("parallel range assignment")};return sum
}
func alias() (int,int) {
 key:=9;p:=(*[1]int)(unsafe.Pointer(&key));var value int
 for key,value=range p {runtime.Gosched()};return key,value
}
func cells(values [3]int,ready,release chan struct{}) ([]*int,[]*int) {
 var keys,elems []*int
 for i,v:=range values {keys=append(keys,&i);elems=append(elems,&v);runtime.Gosched();ready<-struct{}{};<-release}
 return keys,elems
}
type large struct {p *int;pad [257]byte}
func held(ready,release chan struct{}) int {
 values:=[3]large{{p:new(int)},{p:new(int)},{p:new(int)}}
 for i:=range values {*values[i].p=i+5;values[i].pad[256]=byte(i)}
 sum:=0
 for i,v:=range values {values=[3]large{};runtime.Gosched();ready<-struct{}{};<-release;sum+=i+*v.p+int(v.pad[256])}
 return sum
}
func heldPointer(ready,release chan struct{}) int {
 values:=new([3]large)
 for i:=range values {values[i].p=new(int);*values[i].p=i+5;values[i].pad[256]=byte(i)}
 sum:=0
 for i,v:=range values {values=nil;runtime.Gosched();ready<-struct{}{};<-release;sum+=i+*v.p+int(v.pad[256])}
 return sum
}
func control(values [4]int) int {
 sum:=0
 for i,v:=range values {
  for _,w:=range &values {runtime.Gosched();switch w {case 2:continue;case 4:break;default:sum+=w};if w==4 {break}}
  if i==0 {continue};sum+=v;if i==2 {break}
 };return sum
}
func captured(values [3]int) []func() int {
 var result []func() int;for _,v:=range values {runtime.Gosched();result=append(result,func() int{return v})};return result
}
func aggregate(values *[1]large) int {
 i:=0;out:=make([]large,1);for i,out[0]=range values {runtime.Gosched()};return i+*out[0].p
}
func main(){
 if array(&[3]int{1,2,3})!=36||pointer(&[3]int{1,2,3})!=124 {panic("array copy versus pointer snapshot")}
 if named(numbers{1,2,3},arrayPointer(&numbers{4,5,6}))!=21 {panic("named array")}
 if evaluated([3]int{1,2,3})!=6||calls!=1 {panic("array expression evaluation")}
 if keyOnly(nil)!=15||keyCalls([3]int{})!=6||calls!=3||initialized([3]int{1,2,3})!=6||calls!=4 {panic("constant versus nonconstant range length")}
 ch:=make(chan *[3]int,1);ch<-nil;if received(ch)!=3||len(ch)!=0||zero(nil)!=0 {panic("receive or zero-length nil array")}
 if converted(&[3]int{1,2,3})!=9||assignment([3]int{7,8,9})!=3 {panic("array assignment")}
 k,v:=alias();if k!=0||v!=9 {panic("key aliases pointer element")}
 ready,release:=make(chan struct{}),make(chan struct{})
 go func(){for range 9 {<-ready;runtime.GC();release<-struct{}{}}}()
 keys,elems:=cells([3]int{7,8,9},ready,release)
 for i:=range 3 {if *keys[i]!=i||*elems[i]!=i+7 {panic("distinct array iteration cells")}}
 if held(ready,release)!=24||heldPointer(ready,release)!=24||control([4]int{1,2,3,4})!=17 {panic("array lifetime or control")}
 for i,f:=range captured([3]int{4,5,6}) {if f()!=i+4 {panic("array closure fallback")}}
 x:=7;if aggregate(&[1]large{{p:&x}})!=7 {panic("aggregate destination fallback")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"captured", "range variable captured by closure"}, {"aggregate", "aggregate range value with complex destination"}, {"received", "channel site 1: nested receive in range"}},
		[]string{"array", "pointer", "named", "evaluated", "keyCalls", "initialized", "keyOnly", "zero", "converted", "assignment", "alias", "cells", "held", "heldPointer", "control"})
}

func TestArrayRangeBoundaries(t *testing.T) {
	const source = `package main
import "runtime"
type item struct {p *int;pad [257]byte}
var calls int
func values() [3]item {calls++;return [3]item{{p:new(int)},{p:new(int)},{p:new(int)}}}
func byValue() int {n:=0;for i,v:=range values() {runtime.Gosched();n+=i+int(v.pad[256])};return n}
func byKey() int {n:=0;for i:=range values() {runtime.Gosched();n+=i};return n}
func initialized() int {a:=values();n:=0;for i,v:=range a {runtime.Gosched();n+=i+int(v.pad[256])};return n}
func keyInitialized() int {a:=values();n:=0;for i:=range a {runtime.Gosched();n+=i};return n}
func consume(a [3]item) int {return len(a)}
func argument() int {a:=[3]item{};n:=0;for range a {runtime.Gosched();n+=consume(a)};return n}
func nativeValues() [3]item {runtime.Gosched();return [3]item{{p:new(int)},{p:new(int)},{p:new(int)}}}
func nativeInitialized() int {a:=nativeValues();n:=0;for i,v:=range a {runtime.Gosched();n+=i+int(v.pad[256])};return n}
func main(){if byValue()!=3||byKey()!=3||initialized()!=3||keyInitialized()!=3||argument()!=9||nativeInitialized()!=3||calls!=4 {panic("large array transfer")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"byValue", "ordinary aggregate call exceeds stack transfer budget"}, {"byKey", "ordinary aggregate call exceeds stack transfer budget"}, {"initialized", "ordinary aggregate call exceeds stack transfer budget"}, {"keyInitialized", "ordinary aggregate call exceeds stack transfer budget"}, {"argument", "ordinary aggregate call exceeds stack transfer budget"}},
		[]string{"nativeValues", "nativeInitialized"})
}

func TestArrayRangeTerminal(t *testing.T) {
	const source = `package main
import "runtime"
var trace int
func record(v int) {trace=10*trace+v}
func unwind(values *[4]int) (seen any) {
 defer func(){seen=recover()}()
 for _,v:=range values {defer record(v);runtime.Gosched();if v==3 {panic("array")}};return
}
func early(values [3]int) (n int) {for _,v:=range values {defer record(v);runtime.Gosched();n+=v;if v==2 {return}};return}
func exiting(values [3]int,done chan struct{}) {
 defer func(){close(done)}();for _,v:=range &values {defer record(v);runtime.Gosched();if v==2 {runtime.Goexit()}};panic("continued after Goexit")
}
func nilValue(values *[1]int) {for _,v:=range values {runtime.Gosched();_ = v}}
func nilPanic() (seen bool) {defer func(){seen=recover()!=nil}();nilValue(nil);return}
func main(){
 if unwind(&[4]int{1,2,3,4})!="array"||trace!=321 {panic("array panic cleanup")};trace=0
 if early([3]int{1,2,3})!=3||trace!=21 {panic("array early cleanup")};trace=0
 done:=make(chan struct{});go exiting([3]int{1,2,3},done);<-done;if trace!=21 {panic("array Goexit cleanup")}
 if !nilPanic() {panic("nil pointer element load did not panic")};println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"unwind", "early", "exiting", "nilValue"})
}

func TestArrayRangeSpawn(t *testing.T) {
	const source = `package main
import ("runtime";"runtime/debug")
type box struct {n int}
func child(p *box,ready chan int,gate chan struct{},done chan int) {ready<-p.n;<-gate;runtime.Gosched();done<-p.n}
func spawn(values [128]*box,ready chan int,gate chan struct{},done chan int) {
 for _,p:=range values {go child(p,ready,gate,done);values=[128]*box{}}
}
func main(){
 var values [128]*box;for i:=range values {values[i]=&box{i}}
 ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT"&&s.Value=="coro" {native=true}}}
 baseline:=runtime.NumGoroutine();spawn(values,ready,gate,done);values=[128]*box{}
 sum:=0;for range 128 {sum+=<-ready};if sum!=8128 {panic("array go operands")}
 if native&&runtime.NumGoroutine()>baseline+8 {panic("ordinary array tasks")}
 runtime.GC();close(gate);sum=0;for range 128 {sum+=<-done};if sum!=8128 {panic("array spawned element lifetime")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"spawn", "child"})
}

func TestArrayRangeCgo(t *testing.T) {
	testenv.MustHaveCGO(t)
	const source = `package main
/*
#cgo nocallback add
static int add(int n) { return n+10; }
*/
import "C"
//go:noinline
func foreign(values [3]int) int {sum:=0;for _,v:=range values {result:=C.add(C.int(v));sum+=int(result)};return sum}
//go:noinline
func foreignPointer(values *[3]int) int {sum:=0;for _,v:=range values {result:=C.add(C.int(v));sum+=int(result)};return sum}
func main(){if foreign([3]int{1,2,3})!=36||foreignPointer(&[3]int{1,2,3})!=36 {panic("foreign array range")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, nil, []string{"foreign", "foreignPointer"})
}

func TestArrayRangeContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	const source = `package operandcontexts
import ("runtime";"testing";"testing/synctest";"time")
func sleep(values [3]int) {for range values {time.Sleep(time.Hour)}}
func yield(values [3]int) int {n:=0;for _,v:=range values {runtime.Gosched();n+=v};return n}
func TestContexts(t *testing.T) {
 synctest.Test(t,func(t *testing.T){start:=time.Now();sleep([3]int{1,2,3});if time.Since(start)!=3*time.Hour {t.Fatal("array range bubble time")}})
 runtime.LockOSThread();if yield([3]int{1,2,3})!=6 {t.Fatal("locked array range")};runtime.UnlockOSThread()
 if yield([3]int{2,3})!=5 {t.Fatal("array context result")}
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module example.com/arrayrangecontexts\n\ngo 1.28\n", "context_test.go": source} {
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
				if !platform.RaceDetectorSupported(runtime.GOOS, runtime.GOARCH) {
					t.Skip("race detector is not supported")
				}
				testenv.MustHaveCGO(t)
				args = append(args, "-race")
			}
			if mode == "nocoro" {
				experiment, flags = "nocoro", "-l"
			}
			args = append(args, "-gcflags=example.com/arrayrangecontexts="+flags)
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
