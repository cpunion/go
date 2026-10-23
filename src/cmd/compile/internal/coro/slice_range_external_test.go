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

func TestSliceRange(t *testing.T) {
	const source = `package main
import ("runtime";"unsafe")
type number int
type numbers []number
var calls int
func snapshot(v []int) []int {calls++;return v}
func alias() (int,int) {
 key:=9;values:=unsafe.Slice(&key,1);var value int
 for key,value=range values {runtime.Gosched()}
 return key,value
}
var backing []int
func lhsIndex() int {backing[0]=50;return 0}
func lhsMutation() int {
 backing=[]int{9};out:=[]int{0};var value int
 for out[lhsIndex()],value=range backing {runtime.Gosched()}
 return value
}
func mapMutation() int {
 backing=[]int{9};out:=make(map[int]int);var key int
 for key,out[lhsIndex()]=range backing {runtime.Gosched()}
 return key+out[0]
}
func frozen(values []int) int {
 sum:=0
 for i,v:=range snapshot(values) {
  runtime.Gosched()
  sum+=10*i+v
  if i==0 {values[1]=9;values=append(values,100);values=nil}
  i=99;v=99
 }
 return sum
}
func named(values numbers) int {sum:=0;for _,v:=range values {runtime.Gosched();sum+=int(v)};return sum}
func converted(values []int) int {
 var k,v any
 sum:=0
 for k,v=range values {runtime.Gosched();sum+=k.(int)+v.(int)}
 return sum
}
func assignment(values []int) int {
 i:=2;a:=[]int{10,20,30,40};sum:=0
 for i,a[i]=range values {runtime.Gosched();sum+=i}
 if a[0]!=8 || a[1]!=9 || a[2]!=7 || a[3]!=40 {panic("parallel assignment")}
 return sum
}
func empty(values []int) int {
 n:=0
 for range values {runtime.Gosched();n++}
 for _=range values {runtime.Gosched();n++}
 for _,_=range values {runtime.Gosched();n++}
 return n
}
func control(values []int) int {
 sum:=0
 for i,v:=range values {
  for _,w:=range values {
   runtime.Gosched()
   switch w {case 2:continue;case 4:break;default:sum+=w}
   if w==4 {break}
  }
  if i==0 {continue}
  sum+=v
  if i==2 {break}
 }
 return sum
}
func pointers(values []int,ready,release chan struct{}) ([]*int,[]*int) {
 var keys,elems []*int
 for i,v:=range values {
  keys=append(keys,&i);elems=append(elems,&v)
  runtime.Gosched();ready<-struct{}{};<-release
 }
 return keys,elems
}
func valuePointers(values []int) []*int {
 var result []*int
 for _,v:=range values {result=append(result,&v);runtime.Gosched()}
 return result
}
type large struct {p *int;pad [257]byte}
func held(ready,release chan struct{}) int {
 values:=[]large{{p:new(int)},{p:new(int)},{p:new(int)}}
 for i:=range values {*values[i].p=i+5;values[i].pad[256]=byte(i)}
 total:=0
 for i,v:=range values {
  values=nil;runtime.Gosched();ready<-struct{}{};<-release
  total+=*v.p+int(v.pad[256])+i
 }
 return total
}
func zero(values []struct{}) int {n:=0;for i,v:=range values {runtime.Gosched();_ = v;n+=i};return n}
func captured(values []int) []func() int {
 var result []func() int
 for _,v:=range values {runtime.Gosched();result=append(result,func() int{return v})}
 return result
}
func array(values [3]int) int {n:=0;for _,v:=range values {runtime.Gosched();n+=v};return n}
func aggregateAssign(values []large) int {
 index:=2;out:=make([]large,3)
 for index,out[index]=range values {runtime.Gosched()}
 return *out[0].p+*out[1].p+*out[2].p
}
func aggregateKey(values []large) int {
 indices:=[]int{0};var v large
 for indices[0],v=range values {runtime.Gosched()}
 return indices[0]+*v.p
}
func aggregateArray(values [][257]byte) byte {
 index:=0;out:=make([][257]byte,1)
 for index,out[0]=range values {runtime.Gosched()}
 return out[0][256]+byte(index)
}
func main(){
 values:=[]large{{p:new(int)},{p:new(int)},{p:new(int)}};*values[0].p=1;*values[1].p=2;*values[2].p=3
 if aggregateAssign(values)!=6 || aggregateKey(values)!=5 || aggregateArray([][257]byte{{256:7}})!=7 {panic("aggregate destination fallback")}
 key,value:=alias();if key!=0 || value!=9 {panic("range key/element alias ordering")}
 if lhsMutation()!=50 || mapMutation()!=50 {panic("range destination evaluation order")}
 if frozen([]int{1,2,3})!=43 || calls!=1 {panic("frozen header, length or element load")}
 if frozen(nil)!=0 || frozen([]int{})!=0 || calls!=3 {panic("empty source evaluation")}
 if named(numbers{2,3})!=5 || converted([]int{2,3})!=6 || assignment([]int{7,8,9})!=3 {panic("iteration assignment")}
 if empty(nil)!=0 || empty([]int{1,2,3})!=9 || control([]int{1,2,3,4})!=17 {panic("range control")}
 ready,release:=make(chan struct{}),make(chan struct{})
 go func(){for range 3 {<-ready;runtime.GC();release<-struct{}{}}}()
 keys,elems:=pointers([]int{7,8,9},ready,release)
 for i:=range 3 {if *keys[i]!=i || *elems[i]!=i+7 {panic("distinct key/value cells")}}
 for i,p:=range valuePointers([]int{4,5,6}) {if *p!=i+4 {panic("distinct value cell")}}
 go func(){for range 3 {<-ready;runtime.GC();release<-struct{}{}}}()
 if held(ready,release)!=24 || zero(make([]struct{},4))!=6 {panic("typed backing array lifetime")}
 for i,f:=range captured([]int{4,5,6}) {if f()!=i+4 {panic("capture fallback")}}
 if array([3]int{1,2,3})!=6 {panic("array fallback")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"captured", "range variable captured by closure"}, {"aggregateAssign", "aggregate range value with complex destination"}, {"aggregateKey", "aggregate range value with complex destination"}, {"aggregateArray", "aggregate range value with complex destination"}},
		[]string{"array", "lhsMutation", "mapMutation", "alias", "frozen", "named", "converted", "assignment", "empty", "control", "pointers", "valuePointers", "held", "zero"})
}

func TestSliceRangeTerminal(t *testing.T) {
	const source = `package main
import "runtime"
var trace int
func record(v int) {trace=10*trace+v}
func unwind(values []int) (seen any) {
 defer func(){seen=recover()}()
 for _,v:=range values {defer record(v);runtime.Gosched();if v==3 {panic("slice")}}
 return
}
func early(values []int) (n int) {
 for _,v:=range values {defer record(v);runtime.Gosched();n+=v;if v==2 {return}}
 return
}
func exiting(values []int,done chan struct{}) {
 defer func(){close(done)}()
 for _,v:=range values {defer record(v);runtime.Gosched();if v==2 {runtime.Goexit()}}
 panic("continued after Goexit")
}
func main(){
 if unwind([]int{1,2,3,4})!="slice" || trace!=321 {panic("range panic cleanup")};trace=0
 if early([]int{1,2,3})!=3 || trace!=21 {panic("range early return cleanup")};trace=0
 done:=make(chan struct{});go exiting([]int{1,2,3},done);<-done
 if trace!=21 {panic("range Goexit cleanup")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"unwind", "early", "exiting"})
}

func TestSliceRangeSpawn(t *testing.T) {
	const source = `package main
import ("runtime";"runtime/debug")
type box struct {n int}
func child(p *box,ready chan int,gate chan struct{},done chan int) {ready<-p.n;<-gate;runtime.Gosched();done<-p.n}
func spawn(values []*box,ready chan int,gate chan struct{},done chan int) {
 for _,p:=range values {go child(p,ready,gate,done);values=nil}
}
func main(){
 values:=make([]*box,128);for i:=range values {values[i]=&box{i}}
 ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT" && s.Value=="coro" {native=true}}}
 baseline:=runtime.NumGoroutine();spawn(values,ready,gate,done);values=nil
 sum:=0;for range 128 {sum+=<-ready};if sum!=8128 {panic("range go operands")}
 if native && runtime.NumGoroutine()>baseline+8 {panic("ordinary slice tasks")}
 runtime.GC();close(gate);sum=0;for range 128 {sum+=<-done};if sum!=8128 {panic("spawned element lifetime")}
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"spawn", "child"})
}

func TestSliceRangeCgo(t *testing.T) {
	testenv.MustHaveCGO(t)
	const source = `package main
/*
#cgo nocallback add
static int add(int n) { return n+10; }
*/
import "C"
//go:noinline
func foreign(values []int) int {sum:=0;for _,v:=range values {result:=C.add(C.int(v));sum+=int(result)};return sum}
func main(){if foreign([]int{1,2,3})!=36 || foreign(nil)!=0 {panic("foreign slice range")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, nil, []string{"foreign"})
}

func TestSliceRangeContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	const source = `package operandcontexts
import ("runtime";"testing";"testing/synctest";"time")
func sleep(values []int) {for range values {time.Sleep(time.Hour)}}
func yield(values []int) int {n:=0;for _,v:=range values {runtime.Gosched();n+=v};return n}
func TestContexts(t *testing.T) {
 synctest.Test(t,func(t *testing.T){start:=time.Now();sleep([]int{1,2,3});if time.Since(start)!=3*time.Hour {t.Fatal("slice range bubble time")}})
 runtime.LockOSThread();if yield([]int{1,2,3})!=6 {t.Fatal("locked slice range")};runtime.UnlockOSThread()
 if yield([]int{2,3})!=5 {t.Fatal("slice context result")}
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module example.com/slicerangecontexts\n\ngo 1.28\n", "context_test.go": source} {
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
			args = append(args, "-gcflags=example.com/slicerangecontexts="+flags)
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
