// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"internal/buildcfg"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapturedDeclarationStorage(t *testing.T) {
	if !buildcfg.Experiment.Coro {
		t.Skip("requires coroutine experiment")
	}
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime/metrics"
import "runtime"
func integers() (fs []func(int)int, ps []*int) {
 for i:=range 4 {ps=append(ps,&i);fs=append(fs,func(d int)int{i+=d;runtime.Gosched();return i});runtime.Gosched();i+=10};return
}
func slices(values []int) (fs []func(int)int,ps []*int) {
 for i,v:=range values {if i==1 {continue};ps=append(ps,&v);fs=append(fs,func(d int)int{v+=d;runtime.Gosched();return i*100+v});runtime.Gosched();v+=10};return
}
func arrays(values [4]int) (fs []func(int)int,ps []*int) {
 for i,v:=range values {ps=append(ps,&v);fs=append(fs,func(d int)int{v+=d;runtime.Gosched();return i*100+v});runtime.Gosched();v+=10};return
}
func arrayPointers(values *[4]int) (fs []func(int)int,ps []*int) {
 for i,v:=range values {ps=append(ps,&v);fs=append(fs,func(d int)int{v+=d;runtime.Gosched();return i*100+v});runtime.Gosched();v+=10};return
}
func stringsRange(text string) (fs []func()int,keys []*int,values []*rune) {
 for k,v:=range text {keys=append(keys,&k);values=append(values,&v);fs=append(fs,func()int{runtime.Gosched();return k*1000000+int(v)});runtime.Gosched()};return
}
func maps(values map[int]int) (fs []func(int)int,keys,ps []*int) {
 for k,v:=range values {keys=append(keys,&k);ps=append(ps,&v);fs=append(fs,func(d int)int{v+=d;runtime.Gosched();return k*100+v});delete(values,k);runtime.Gosched();v+=10};return
}
func channels(values chan int) (fs []func(int)int,ps []*int) {
 for v:=range values {ps=append(ps,&v);fs=append(fs,func(d int)int{v+=d;runtime.Gosched();return v});runtime.Gosched();v+=10};return
}
func assigned(values []int) (fs []func(int)int,ps []*int) {
 var v int
 for _,v=range values {ps=append(ps,&v);fs=append(fs,func(d int)int{v+=d;runtime.Gosched();return v});runtime.Gosched()};return
}
type object struct {n int;pad [2048]byte}
func pointers(values []*object,ready,release chan int) (fs []func()int) {
 for _,p:=range values {fs=append(fs,func()int{ready<-p.n;<-release;runtime.Gosched();return p.n});runtime.Gosched()};return
}
func cases(values []any) (fs []func()any,ps []any) {
 for _,item:=range values {switch x:=item.(type) {
 case int:
  ps=append(ps,&x);fs=append(fs,func()any{runtime.Gosched();return x});runtime.Gosched();x+=10
 case *object:
  ps=append(ps,&x);fs=append(fs,func()any{runtime.Gosched();return x});runtime.Gosched();x=&object{n:x.n+10}
 case nil,string:
  ps=append(ps,&x);fs=append(fs,func()any{runtime.Gosched();return x});runtime.Gosched();x="bound"
 default:
  ps=append(ps,&x);fs=append(fs,func()any{runtime.Gosched();return x});runtime.Gosched();x="default"
 }};return
}
func spawn(values []int,ready,release,done chan int) {
 for _,v:=range values {go func(){ready<-v;<-release;runtime.Gosched();done<-v}()}
}
func cleanup(values []int) (ps []*int) {
 defer func(){if recover()!="cells" {panic("range cleanup recovery")}}()
 for _,v:=range values {ps=append(ps,&v);defer func(){v+=100}();runtime.Gosched();if v==2 {panic("cells")}};return
}
func distinct(ps []*int) {for i,p:=range ps {for j,q:=range ps {if i!=j && p==q {panic("aliased declaration cells")}}}}
func check(fs []func(int)int,ps []*int,indices,values []int) {
 if len(fs)!=len(values)||len(ps)!=len(values) {panic("range length")};distinct(ps)
 for i,f:=range fs {if *ps[i]!=values[i]+10||f(2)!=indices[i]*100+values[i]+12||*ps[i]!=values[i]+12 {panic("closure and pointer disagree")}}
}
func main(){
 fs,ps:=integers();check(fs,ps,[]int{0,0,0,0},[]int{0,1,2,3})
 fs,ps=slices([]int{4,5,6,7});check(fs,ps,[]int{0,2,3},[]int{4,6,7})
 fs,ps=arrays([4]int{4,5,6,7});check(fs,ps,[]int{0,1,2,3},[]int{4,5,6,7})
 fs,ps=arrayPointers(&[4]int{4,5,6,7});check(fs,ps,[]int{0,1,2,3},[]int{4,5,6,7})
 readers,keys,runes:=stringsRange("Aé\xff𐀀");indices:=[]int{0,1,3,4};codepoints:=[]rune{'A','é','\ufffd','𐀀'};distinct(keys)
 if len(readers)!=4 {panic("string length")};for i,f:=range readers {if *keys[i]!=indices[i]||*runes[i]!=codepoints[i]||f()!=indices[i]*1000000+int(codepoints[i]) {panic("UTF8 capture")};for j:=0;j<i;j++ {if runes[j]==runes[i] {panic("aliased rune cells")}}}
 m:=map[int]int{1:4,2:5,3:6};var mapKeys []*int;fs,mapKeys,ps=maps(m);distinct(mapKeys);distinct(ps)
 if len(m)!=0||len(fs)!=3 {panic("map deletion")};for i,f:=range fs {k:=*mapKeys[i];if f(2)!=k*100+k+15||*ps[i]!=k+15 {panic("map capture")}}
 ch:=make(chan int,4);for i:=range 4 {ch<-i};close(ch);fs,ps=channels(ch);check(fs,ps,[]int{0,0,0,0},[]int{0,1,2,3})
 fs,ps=assigned([]int{4,5,6});for i,f:=range fs {if ps[i]!=ps[0]||f(1)!=7+i||*ps[0]!=7+i {panic("assignment range lost shared cell")}}
 objects:=[]*object{{n:11},{n:12},{n:13}};ready,release:=make(chan int),make(chan int);getters:=pointers(objects,ready,release);objects=nil;runtime.GC()
 for i,f:=range getters {go func(){if <-ready!=11+i {panic("parked cell identity")};runtime.GC();release<-1}();if f()!=11+i {panic("pointer capture GC lifetime")}}
 anyReaders,anyPointers:=cases([]any{1,2,&object{n:3},&object{n:4},nil,"ab",true,5.0});runtime.GC()
 if anyPointers[0].(*int)==anyPointers[1].(*int)||anyPointers[2].(**object)==anyPointers[3].(**object)||anyPointers[4].(*any)==anyPointers[5].(*any)||anyPointers[6].(*any)==anyPointers[7].(*any) {panic("aliased case declarations")}
 for i,f:=range anyReaders {v:=f();switch i {case 0,1:if v.(int)!=11+i||*anyPointers[i].(*int)!=11+i {panic("int case capture")};case 2,3:if v.(*object).n!=11+i||(*anyPointers[i].(**object)).n!=11+i {panic("pointer case capture")};case 4,5:if v!="bound"||*anyPointers[i].(*any)!="bound" {panic("multi-type case capture")};default:if v!="default"||*anyPointers[i].(*any)!="default" {panic("default case capture")}}}
 done:=make(chan int);before:=ordinaryGoroutines();values:=make([]int,64);for i:=range values {values[i]=i};spawn(values,ready,release,done)
 total:=0;for range 64 {total+=<-ready};if total!=2016 {panic("spawn input cells")};if NATIVE && ordinaryGoroutines()>before+16 {panic("ordinary goroutine per range child")};runtime.GC()
 for range 64 {release<-1};total=0;for range 64 {total+=<-done};if total!=2016 {panic("spawn output cells")}
 ps=cleanup([]int{1,2,3});if len(ps)!=2||ps[0]==ps[1]||*ps[0]!=101||*ps[1]!=102 {panic("panic cleanup cell lifetime")}
 println("captured-storage-ok")
}
// Count ordinary Go stacks separately from unhosted logical goroutines.
// This remains a dispatch check after NumGoroutine includes native tasks.
func ordinaryGoroutines() int {
 samples := []metrics.Sample{{Name: "/sched/coro/goroutines/unhosted:goroutines"}}
 metrics.Read(samples)
 native := 0
 if samples[0].Value.Kind() == metrics.KindUint64 { native = int(samples[0].Value.Uint64()) }
 return runtime.NumGoroutine() - native
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			program := strings.ReplaceAll(source, "NATIVE", map[bool]string{true: "true", false: "false"}[config.experiment == "coro"])
			if err := os.WriteFile(path, []byte(program), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "program")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", binary, path)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			out, err := build.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"integers", "slices", "arrays", "arrayPointers", "stringsRange", "maps", "channels", "assigned", "pointers", "cases", "spawn", "spawn.func1", "cleanup"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission\n%s", name, out)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "captured-storage-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
