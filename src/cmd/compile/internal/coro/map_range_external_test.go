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

func TestMapRange(t *testing.T) {
	const source = `package main
import "runtime"
type table map[int]int
var calls int
func expression(m map[int]int) map[int]int {calls++;return m}
func each(m map[int]int) int {n:=0;for k,v:=range expression(m) {runtime.Gosched();n+=k+v;m=nil;k=999;v=999};return n}
func named(m table) int {n:=0;for k,v:=range m {runtime.Gosched();n+=k+v};return n}
func keys(m map[int]int) int {n:=0;for k:=range m {runtime.Gosched();n+=k;k=999};return n}
func keyless(m map[int]int) int {n:=0;for range m {runtime.Gosched();n++};for _,_=range m {runtime.Gosched();n++};return n}
func converted(m map[int]int) int {var k,v any;n:=0;for k,v=range m {runtime.Gosched();n+=k.(int)+v.(int)};return n}
type name string
type number int
func strings(m map[name]*number) int {n:=0;for k,v:=range m {runtime.Gosched();n+=len(k)+int(*v)};return n}
func interfaces(m map[any]any) int {n:=0;for k,v:=range m {runtime.Gosched();n+=k.(int)+v.(int)};return n}
func zero(m map[struct{}]struct{}) int {n:=0;for k,v:=range m {runtime.Gosched();if k!=v {panic("zero-sized map entry")};n++};return n}
func assignment(m map[int]int) {k:=2;out:=make([]int,8);for k,out[k]=range m {runtime.Gosched()};if k!=3||out[2]!=7 {panic("map parallel assignment")}}
func destination(m map[int]int,out []int) []int {m[3]=9;return out}
func operandOrder(m map[int]int) int {out:=make([]int,1);v:=0;for destination(m,out)[0],v=range m {runtime.Gosched()};return out[0]+v}
func singleDestination(m map[int]int) int {out:=make([]int,1);for out[0]=range m {runtime.Gosched()};return out[0]}
func alias(m map[int]int) int {v:=0;for v,v=range m {runtime.Gosched()};return v}
func cells(m map[int]int) ([]*int,[]*int) {var ks,vs []*int;for k,v:=range m {ks=append(ks,&k);vs=append(vs,&v);runtime.Gosched()};return ks,vs}
func control(m map[int]int) int {n:=0;for k,v:=range m {runtime.Gosched();switch k {case 0:continue;case 1:n+=v;default:n+=v};if n<0 {break}};return n}
func nested(m map[int]int) int {n:=0;for range 3 {for k,v:=range m {runtime.Gosched();n+=k+v};for range map[int]int(nil) {n=999}};return n}
func captured(m map[int]int) []func() int {var fs []func() int;for k,v:=range m {runtime.Gosched();fs=append(fs,func() int{return k+v})};return fs}
func labeled(m map[int]int) int {n:=0;outer:for range m {runtime.Gosched();n++;break outer};return n}
func received(ch chan map[int]int) int {n:=0;for range <-ch {runtime.Gosched();n++};return n}
func main(){
 for _,m:=range []map[int]int{nil,{}, {1:10,2:20,3:30}} {
  want,ks:=0,0;for k,v:=range m {want+=k+v;ks+=k}
  if each(m)!=want||named(table(m))!=want||keys(m)!=ks||keyless(m)!=2*len(m)||converted(m)!=want||nested(m)!=3*want {panic("map range values")}
  a,b:=cells(m);sum:=0;for i:=range a {sum+=*a[i]+*b[i]};if sum!=want {panic("distinct map cells")}
  sum=0;for _,f:=range captured(m) {sum+=f()};if sum!=want {panic("captured map fallback")}
 }
 n:=number(10);if strings(map[name]*number{"abc":&n})!=13||interfaces(map[any]any{1:2,3:4})!=10||zero(map[struct{}]struct{}{{}:{}})!=1 {panic("map key or value types")};assignment(map[int]int{3:7});if operandOrder(map[int]int{3:7})!=12||singleDestination(map[int]int{3:7})!=3 {panic("map operand evaluation")};if alias(map[int]int{3:7})!=7||calls!=3||control(map[int]int{0:2,1:3,2:4})!=7 {panic("map assignment or control")}
 ch:=make(chan map[int]int,1);ch<-map[int]int{1:2};if received(ch)!=1||labeled(map[int]int{1:2})!=1 {panic("map fallback")};println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, []switchFallback{{"captured", "range variable captured by closure"}, {"labeled", "control flow LABEL"}, {"received", "channel site 1: nested receive in range"}}, []string{"each", "named", "keys", "keyless", "converted", "strings", "interfaces", "zero", "assignment", "operandOrder", "singleDestination", "alias", "cells", "control", "nested"})
}

func TestMapRangeMutation(t *testing.T) {
	const source = `package main
import ("runtime";"math")
func deleted(m map[int]int) int {n:=0;for range m {runtime.Gosched();n++;for k:=range m {delete(m,k)}};return n}
func updated(m map[int]int) int {n:=0;first:=true;for k,v:=range m {runtime.Gosched();if first {first=false;for other:=range m {if other!=k {m[other]=100}}} else if v!=100 {panic("map update invisible")};n++};return n}
func grown(m map[int]int,original int) int {
 seen:=make(map[int]bool);first:=true;n:=0
 for k,v:=range m {
  runtime.Gosched();if seen[k]||v!=k+1 {panic("map grow duplicate or stale value")};seen[k]=true;if k<original {n++}
  if first {first=false;for i:=original;i<original+4096;i++ {m[i]=i+1}}
 };if len(seen)>original+4096 {panic("map growth iteration bound")};return n
}
func cleared(m map[float64]int) int {n:=0;for range m {runtime.Gosched();n++;clear(m);for i:=0;i<4096;i++ {m[float64(i)]=i};clear(m)};return n}
func main(){for range 16 {for _,size:=range []int{1,7,8,9,64,1024} {
 m:=make(map[int]int);for i:=0;i<size;i++ {m[i]=i+1};if grown(m,size)!=size {panic("map lost original key")}
 m=make(map[int]int);for i:=0;i<size;i++ {m[i]=i};if updated(m)!=size||deleted(m)!=1||len(m)!=0 {panic("map deletion or update")}
 f:=make(map[float64]int);for i:=0;i<size;i++ {f[math.NaN()]=i};if cleared(f)!=1||len(f)!=0 {panic("NaN clear during iteration")}
 }};println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"deleted", "updated", "grown", "cleared"})
}

func TestMapRangeLifetime(t *testing.T) {
	const source = `package main
import "runtime"
type key struct {p *int;pad [256]byte}
type value struct {p *int;pad [2048]byte}
func input() *map[key]value {m:=make(map[key]value);for i:=0;i<32;i++ {a,b:=new(int),new(int);*a=i;*b=100+i;k:=key{p:a};k.pad[255]=byte(i);v:=value{p:b};v.pad[2047]=byte(i);m[k]=v};return &m}
func held(p *map[key]value,ready,release chan struct{}) int {n:=0;for k,v:=range *p {
 *p=nil;ready<-struct{}{};<-release;runtime.Gosched()
 if int(k.pad[255])!=*k.p||int(v.pad[2047])+100!=*v.p {panic("map indirect entry lifetime")};n+=*k.p+*v.p
 };return n}
func main(){ready,release,done:=make(chan struct{}),make(chan struct{}),make(chan int);p:=input();go func(){done<-held(p,ready,release)}();count:=0
 for {select {case <-ready:
  if *p!=nil {panic("map source reference remains")};scratch:=make([][]byte,32);for i:=range scratch {scratch[i]=make([]byte,65536);scratch[i][0]=77};runtime.GC();release<-struct{}{};runtime.KeepAlive(scratch);count++
 case n:=<-done:if n!=4192||count!=32 {panic("map GC checksum")};println("switch-return-ok");return}}
}`
	testSwitchReturnProgram(t, source, nil, []string{"held"})
}

func TestMapRangeTerminal(t *testing.T) {
	const source = `package main
import "runtime"
var trace int
func record(v int) {trace=trace*10+v}
func unwind(m map[int]int) (seen any) {defer func(){seen=recover()}();for _,v:=range m {defer record(v);runtime.Gosched();panic("map")};return}
func early(m map[int]int) (n int) {for _,v:=range m {defer record(v);runtime.Gosched();n++;return};return}
func exiting(m map[int]int,done chan struct{}) {defer func(){close(done)}();for _,v:=range m {defer record(v);runtime.Gosched();runtime.Goexit()};panic("continued after Goexit")}
func header() map[int]int {panic("header")}
func headerPanic() (seen any) {defer func(){seen=recover()}();for range header() {runtime.Gosched()};return}
func main(){m:=map[int]int{1:7};if unwind(m)!="map"||trace!=7 {panic("map panic cleanup")};trace=0;if early(m)!=1||trace!=7 {panic("map early cleanup")};trace=0
 done:=make(chan struct{});go exiting(m,done);<-done;if trace!=7||headerPanic()!="header" {panic("map exit or header cleanup")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, []switchFallback{{"headerPanic", "nested await site 2"}}, []string{"unwind", "early", "exiting"})
}

func TestMapRangeBoundaries(t *testing.T) {
	const source = `package main
import "runtime"
func complexKey(m map[[256]byte]int) {var keys [1][256]byte;for keys[0]=range m {runtime.Gosched()}}
func complexValue(m map[int][256]byte) {var values [1][256]byte;for _,values[0]=range m {runtime.Gosched()}}
func main(){complexKey(map[[256]byte]int{{1}:2});complexValue(map[int][256]byte{1:{2}});println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, []switchFallback{{"complexKey", "aggregate map range operand with complex destination"}, {"complexValue", "aggregate map range operand with complex destination"}}, nil)
}

// A map range must not admit a resume with large late assertion temporaries.
// Large interface conversions remain native when assertions run in the caller.
func TestMapRangeAssertions(t *testing.T) {
	const source = `package main
import "runtime"
type key [8192]byte
type value [16384]byte
func large(m map[key]value) int {var k,v any;n:=0;for k,v=range m {runtime.Gosched();a,b:=k.(key),v.(value);n+=int(a[8191])+int(b[16383])};return n}
func converted(m map[key]value,out chan [2]any) {var k,v any;for k,v=range m {runtime.Gosched();out<-[2]any{k,v}};close(out)}
func small(m map[[16]byte][32]byte) int {var k,v any;n:=0;for k,v=range m {runtime.Gosched();a,b:=k.([16]byte),v.([32]byte);n+=int(a[15])+int(b[31])};return n}
func boundary(m map[int]any) int {n:=0;for _,v:=range m {runtime.Gosched();a:=v.([100]byte);n+=int(a[99])};return n}
func beyond(m map[int]any) int {n:=0;for _,v:=range m {runtime.Gosched();a:=v.([101]byte);n+=int(a[100])};return n}
func combined(m map[int]any) int {n:=0;for _,v:=range m {runtime.Gosched();a,b:=v.([64]byte),v.([64]byte);n+=int(a[63])+int(b[63])};return n}
func commaOK(m map[int]any) int {n:=0;for _,v:=range m {runtime.Gosched();a,ok:=v.([128]byte);if ok {n+=int(a[127])}};return n}
func structure(m map[int]any) int {n:=0;for _,v:=range m {runtime.Gosched();a:=v.(struct{pad [128]byte});n+=int(a.pad[127])};return n}
func main(){var k key;var v value;k[8191]=3;v[16383]=7;m:=map[key]value{k:v};if large(m)!=10 {panic("large assertion fallback")};out:=make(chan [2]any);go converted(m,out);n:=0;for pair:=range out {a,b:=pair[0].(key),pair[1].(value);n+=int(a[8191])+int(b[16383])};if n!=10 {panic("large conversion")}
 var a [16]byte;var b [32]byte;a[15]=3;b[31]=7;if small(map[[16]byte][32]byte{a:b})!=10 {panic("small native assertions")}
 var c [100]byte;var d [101]byte;var e [64]byte;var f [128]byte;c[99]=1;d[100]=2;e[63]=3;f[127]=4
 if boundary(map[int]any{0:c})!=1||beyond(map[int]any{0:d})!=2||combined(map[int]any{0:e})!=6||commaOK(map[int]any{0:f,1:1})!=4||structure(map[int]any{0:struct{pad [128]byte}{pad:f}})!=4 {panic("aggregate assertion bounds")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, []switchFallback{{"large", "aggregate type assertion exceeds stack transfer budget"}, {"beyond", "aggregate type assertion exceeds stack transfer budget"}, {"combined", "aggregate type assertion exceeds stack transfer budget"}, {"commaOK", "aggregate type assertion exceeds stack transfer budget"}, {"structure", "aggregate type assertion exceeds stack transfer budget"}}, []string{"converted", "small", "boundary"})
}

func TestMapRangeImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/maprange\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
type Text map[int]int
func Sum(s Text) int {n:=0;for k,v:=range s {runtime.Gosched();n+=k+v};return n}
func Header(s Text) Text {return s}
`,
		"main.go": `package main
import ("runtime";"example.com/maprange/leaf")
func imported(s leaf.Text) int {runtime.Gosched();n:=leaf.Sum(s);for _,v:=range leaf.Header(s) {runtime.Gosched();n+=int(v)};return n}
func main(){if imported(leaf.Text{1:10,2:20})!=63 {panic("imported map range")};println("switch-return-ok")}
`,
	}
	t.Run("native-leaf", func(t *testing.T) {
		testSwitchReturnFiles(t, files, "example.com/maprange/...=", nil, []string{"imported"})
	})
	t.Run("default-leaf", func(t *testing.T) { testSwitchReturnFiles(t, files, "", nil, nil) })
}

func TestMapRangeSpawn(t *testing.T) {
	const source = `package main
import ("runtime";"runtime/debug")
func child(k,v int,ready chan int,gate chan struct{},done chan int) {ready<-k+v;<-gate;runtime.Gosched();done<-k+v}
func spawn(m map[int]int,ready chan int,gate chan struct{},done chan int) {for k,v:=range m {go child(k,v,ready,gate,done);m=nil}}
func main(){m:=make(map[int]int);for i:=0;i<128;i++ {m[i]=i+128};ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,s:=range info.Settings {if s.Key=="GOEXPERIMENT"&&s.Value=="coro" {native=true}}}
 baseline:=runtime.NumGoroutine();spawn(m,ready,gate,done);m=nil;sum:=0;for range 128 {sum+=<-ready};if sum!=32640 {panic("map go operands")}
 if native&&runtime.NumGoroutine()>baseline+8 {panic("ordinary map tasks")};runtime.GC();close(gate);sum=0;for range 128 {sum+=<-done};if sum!=32640 {panic("map child lifetime")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, nil, []string{"spawn", "child"})
}

func TestMapRangeCgo(t *testing.T) {
	testenv.MustHaveCGO(t)
	const source = `package main
/*
#cgo nocallback add
static int add(int n) { return n+10; }
*/
import "C"
//go:noinline
func foreign(s map[int]int) int {n:=0;for _,v:=range s {result:=C.add(C.int(v));n+=int(result)};return n}
//go:noinline
func foreignKey(s map[int]int) int {n:=0;for k:=range s {result:=C.add(C.int(k));n+=int(result)};return n}
func main(){if foreign(map[int]int{1:10,2:20,3:30})!=90||foreignKey(map[int]int{1:10,2:20,3:30})!=36 {panic("foreign map range")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, nil, []string{"foreign", "foreignKey"})
}

func TestMapRangeContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	const source = `package stringcontexts
import ("runtime";"testing";"testing/synctest";"time")
func sleep(s map[int]int) {for range s {time.Sleep(time.Hour)}}
func yield(s map[int]int) int {n:=0;for _,v:=range s {runtime.Gosched();n+=int(v)};return n}
func TestContexts(t *testing.T) {
 synctest.Test(t,func(t *testing.T){start:=time.Now();sleep(map[int]int{1:10,2:20,3:30});if time.Since(start)!=3*time.Hour {t.Fatal("map range bubble time")}})
 runtime.LockOSThread();if yield(map[int]int{1:10,2:20,3:30})!=60 {t.Fatal("locked map range")};runtime.UnlockOSThread()
 if yield(map[int]int{1:10,2:20})!=30 {t.Fatal("map context result")}
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module example.com/maprangecontexts\n\ngo 1.28\n", "context_test.go": source} {
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
			args = append(args, "-gcflags=example.com/maprangecontexts="+flags)
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
