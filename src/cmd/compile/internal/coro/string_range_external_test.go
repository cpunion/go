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

func TestStringRange(t *testing.T) {
	const source = `package main
import ("runtime";"unicode/utf8")
type text string
var calls int
func expression(s string) string {calls++;return s}
func each(s string) ([]int,[]rune) {
 var keys []int;var values []rune
 for k,v:=range expression(s) {runtime.Gosched();keys=append(keys,k);values=append(values,v);s="";k=999;v=999}
 return keys,values
}
func named(s text) int {n:=0;for _,v:=range s {runtime.Gosched();n+=int(v)};return n}
func keys(s string) []int {var result []int;for k:=range s {runtime.Gosched();result=append(result,k);k=999};return result}
func keyless(s string) int {n:=0;for range s {runtime.Gosched();n++};for _,_=range s {runtime.Gosched();n++};return n}
func converted(s string) int {var k,v any;n:=0;for k,v=range s {runtime.Gosched();n+=k.(int)+int(v.(rune))};return n}
func assignment(s string) {
 k:=2;out:=make([]rune,8)
 for k,out[k]=range s {runtime.Gosched()}
 if k!=6||out[2]!='A'||out[0]!='¢'||out[1]!='中'||out[3]!='😀' {panic("parallel string assignment")}
}
func cells(s string,ready,release chan struct{}) ([]*int,[]*rune) {
 var keys []*int;var values []*rune
 for k,v:=range s {keys=append(keys,&k);values=append(values,&v);s="";runtime.Gosched();ready<-struct{}{};<-release}
 return keys,values
}
func held(s string,ready,release chan struct{}) int {
 n:=0;for k,v:=range s {s="";runtime.Gosched();ready<-struct{}{};<-release;n+=k+int(v)};return n
}
func control(s string) int {
 n:=0;for k,v:=range s {runtime.Gosched();switch k {case 0:continue;case 3:return n+int(v)};n+=int(v)};return n
}
func nested(s string) int {n:=0;for range s {for _,v:=range "中😀" {runtime.Gosched();if v=='中' {continue};n++}};return n}
func received(ch chan string) int {n:=0;for range <-ch {runtime.Gosched();n++};return n}
func captured(s string) []func() rune {var fs []func() rune;for _,v:=range s {runtime.Gosched();fs=append(fs,func() rune{return v})};return fs}
func labeled(s string) int {n:=0;outer:for range s {runtime.Gosched();n++;break outer};return n}
func reference(s string) ([]int,[]rune) {
 var keys []int;var values []rune
 for k:=0;k<len(s); {v,width:=utf8.DecodeRuneInString(s[k:]);keys=append(keys,k);values=append(values,v);k+=width};return keys,values
}
func main(){
 cases:=[]string{"","\x00Az\x7f","¢中😀","\xff\xc0\xaf\xe2\x82","\xef\xbf\xbd","A¢\xff中\xed\xa0\x80😀\xf4\x90\x80\x80\x80"}
 for _,s:=range cases {
  wantKeys,wantValues:=reference(s);gotKeys,gotValues:=each(s);gotOnly:=keys(s)
  if len(gotKeys)!=len(wantKeys)||len(gotValues)!=len(wantValues)||len(gotOnly)!=len(wantKeys)||keyless(s)!=2*len(wantKeys) {panic("string rune count")}
  sum:=0;for i:=range wantKeys {if gotKeys[i]!=wantKeys[i]||gotOnly[i]!=wantKeys[i]||gotValues[i]!=wantValues[i] {panic("UTF-8 offsets or values")};sum+=wantKeys[i]+int(wantValues[i])}
  if converted(s)!=sum {panic("string interface assignment")}
 }
 if calls!=len(cases)||named(text("A¢中"))!=20240||control("A¢中Z")!=20175||nested("A¢")!=2 {panic("string evaluation or control")}
 assignment("A¢中😀")
 ready,release:=make(chan struct{}),make(chan struct{})
 go func(){for range 8 {<-ready;runtime.GC();release<-struct{}{}}}()
 s:=string([]byte("A¢中😀"));ks,vs:=cells(s,ready,release)
 wantKeys,wantValues:=reference(s);s=""
 for i:=range wantKeys {if *ks[i]!=wantKeys[i]||*vs[i]!=wantValues[i] {panic("distinct string iteration cells")}}
 if held(string([]byte("A¢中😀")),ready,release)!=148762 {panic("string snapshot lifetime")}
 ch:=make(chan string,1);ch<-"A¢中";if received(ch)!=3||len(ch)!=0 {panic("range receive fallback")}
 for i,f:=range captured("A¢中😀") {if f()!=wantValues[i] {panic("captured string fallback")}}
 if labeled("A¢中")!=1 {panic("labeled string fallback")};println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source,
		[]switchFallback{{"captured", "range variable captured by closure"}, {"labeled", "control flow LABEL"}, {"received", "channel site 1: nested receive in range"}},
		[]string{"each", "named", "keys", "keyless", "converted", "assignment", "cells", "held", "control", "nested"})
}

func TestStringRangeTerminal(t *testing.T) {
	const source = `package main
import "runtime"
var trace string
func record(v rune) {trace+=string(v)}
func unwind(s string) (seen any) {
 defer func(){seen=recover()}();for _,v:=range s {defer record(v);runtime.Gosched();if v=='中' {panic("string")}};return
}
func early(s string) (n int) {for _,v:=range s {defer record(v);runtime.Gosched();n++;if v=='¢' {return}};return}
func exiting(s string,done chan struct{}) {
 defer func(){close(done)}();for _,v:=range s {defer record(v);runtime.Gosched();if v=='¢' {runtime.Goexit()}};panic("continued after Goexit")
}
func header() string {panic("header")}
func headerPanic() (seen any) {defer func(){seen=recover()}();for range header() {runtime.Gosched()};return}
func main(){
 if unwind("A¢中😀")!="string"||trace!="中¢A" {panic("string panic cleanup")};trace=""
 if early("A¢中😀")!=2||trace!="¢A" {panic("string early cleanup")};trace=""
 done:=make(chan struct{});go exiting("A¢中😀",done);<-done;if trace!="¢A" {panic("string Goexit cleanup")}
 if headerPanic()!="header" {panic("string header panic ownership")};println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, []switchFallback{{"headerPanic", "nested await site 2"}}, []string{"unwind", "early", "exiting"})
}

func TestStringRangeUTF8(t *testing.T) {
	const source = `package main
import ("runtime";"unicode/utf8")
func scan(s string) uint64 {
 h:=uint64(1469598103934665603)
 for k,v:=range s {runtime.Gosched();h=(h^uint64(k))*1099511628211;h=(h^uint64(v))*1099511628211};return h
}
func reference(s string) uint64 {
 h:=uint64(1469598103934665603)
 for k:=0;k<len(s); {v,width:=utf8.DecodeRuneInString(s[k:]);h=(h^uint64(k))*1099511628211;h=(h^uint64(v))*1099511628211;k+=width};return h
}
func check(s string) {if scan(s)!=reference(s) {panic("UTF-8 differential")}}
func main(){
 check("");for a:=0;a<256;a++ {check(string([]byte{byte(a)}));for b:=0;b<256;b++ {check(string([]byte{byte(a),byte(b)}))}}
 edges:=[]byte{0,0x7f,0x80,0xbf,0xc0,0xff}
 for a:=0xe0;a<=0xef;a++ {for b:=0;b<256;b++ {for _,c:=range edges {check(string([]byte{byte(a),byte(b),c}))}}}
 for a:=0xf0;a<=0xf7;a++ {for b:=0;b<256;b++ {for _,c:=range edges {for _,d:=range edges {check(string([]byte{byte(a),byte(b),c,d}))}}}}
 for _,r:=range []rune{0,0x7f,0x80,0x7ff,0x800,0xd7ff,0xe000,0xffff,0x10000,utf8.MaxRune} {
  s:=string(r);check(s);check("A"+s+"中");for n:=0;n<len(s);n++ {check(s[:n]);check(s[n:])}
 }
 println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"scan"})
}

func TestStringRangeImported(t *testing.T) {
	files := map[string]string{
		"go.mod": "module example.com/stringrange\n\ngo 1.28\n",
		"leaf/leaf.go": `package leaf
import "runtime"
type Text string
func Sum(s Text) int {n:=0;for k,v:=range s {runtime.Gosched();n+=k+int(v)};return n}
func Header(s Text) Text {return s}
`,
		"main.go": `package main
import ("runtime";"example.com/stringrange/leaf")
func imported(s leaf.Text) int {runtime.Gosched();n:=leaf.Sum(s);for _,v:=range leaf.Header(s) {runtime.Gosched();n+=int(v)};return n}
func main(){if imported(leaf.Text("A¢中"))!=40484 {panic("imported string range")};println("switch-return-ok")}
`,
	}
	t.Run("native-leaf", func(t *testing.T) {
		testSwitchReturnFiles(t, files, "example.com/stringrange/...=", nil, []string{"imported"})
	})
	t.Run("default-leaf", func(t *testing.T) { testSwitchReturnFiles(t, files, "", nil, nil) })
}

func TestStringRangeSpawn(t *testing.T) {
	const source = `package main
import ("runtime";"runtime/debug")
func child(k int,v rune,ready chan int,gate chan struct{},done chan int) {ready<-k+int(v);<-gate;runtime.Gosched();done<-k+int(v)}
func spawn(s string,ready chan int,gate chan struct{},done chan int) {for k,v:=range s {go child(k,v,ready,gate,done);s=""}}
func main(){
 values:=make([]rune,128);for i:=range values {values[i]=rune(i+128)};s:=string(values);values=nil
 ready,gate,done:=make(chan int,128),make(chan struct{}),make(chan int,128)
 native:=false;if info,ok:=debug.ReadBuildInfo();ok {for _,setting:=range info.Settings {if setting.Key=="GOEXPERIMENT"&&setting.Value=="coro" {native=true}}}
 baseline:=runtime.NumGoroutine();spawn(s,ready,gate,done);s=""
 sum:=0;for range 128 {sum+=<-ready};if sum!=40768 {panic("string go operands")}
 if native&&runtime.NumGoroutine()>baseline+8 {panic("ordinary string tasks")}
 runtime.GC();close(gate);sum=0;for range 128 {sum+=<-done};if sum!=40768 {panic("string child lifetime")};println("switch-return-ok")
}`
	testSwitchReturnProgram(t, source, nil, []string{"spawn", "child"})
}

func TestStringRangeCgo(t *testing.T) {
	testenv.MustHaveCGO(t)
	const source = `package main
/*
#cgo nocallback add
static int add(int n) { return n+10; }
*/
import "C"
//go:noinline
func foreign(s string) int {n:=0;for _,v:=range s {result:=C.add(C.int(v));n+=int(result)};return n}
//go:noinline
func foreignKey(s string) int {n:=0;for k:=range s {result:=C.add(C.int(k));n+=int(result)};return n}
func main(){if foreign("A¢中")!=20270||foreignKey("A¢中")!=34 {panic("foreign string range")};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, nil, []string{"foreign", "foreignKey"})
}

func TestStringRangeContexts(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	const source = `package stringcontexts
import ("runtime";"testing";"testing/synctest";"time")
func sleep(s string) {for range s {time.Sleep(time.Hour)}}
func yield(s string) int {n:=0;for _,v:=range s {runtime.Gosched();n+=int(v)};return n}
func TestContexts(t *testing.T) {
 synctest.Test(t,func(t *testing.T){start:=time.Now();sleep("A¢中");if time.Since(start)!=3*time.Hour {t.Fatal("string range bubble time")}})
 runtime.LockOSThread();if yield("A¢中")!=20240 {t.Fatal("locked string range")};runtime.UnlockOSThread()
 if yield("A¢")!=227 {t.Fatal("string context result")}
}
`
	dir := t.TempDir()
	for name, data := range map[string]string{"go.mod": "module example.com/stringrangecontexts\n\ngo 1.28\n", "context_test.go": source} {
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
			args = append(args, "-gcflags=example.com/stringrangecontexts="+flags)
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
