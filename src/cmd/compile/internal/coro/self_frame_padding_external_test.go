// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import (
	"encoding/binary"
	"internal/testenv"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSelfFrameHeaderPadding(t *testing.T) {
	testenv.MustHaveGoBuild(t)
	if !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64" || runtime.GOOS == "darwin" && runtime.GOARCH == "arm64") {
		t.Skip("native coroutine executor is not supported")
	}
	const source = `package main
import "runtime"
var request=make(chan bool)
var done=make(chan bool)
var order int
type item struct {value int}
var churn []*item
func flag(tag int,value bool)bool {order=order*10+tag;return value}
func pointer(tag int,value *int)*int {order=order*10+tag;return value}
func depth(tag int,value int)int {order=order*10+tag;return value}
func collect(){for range request {
 runtime.GC()
 churn=make([]*item,8192)
 for i:=range churn {churn[i]=&item{-100000}}
 runtime.GC()
 done<-true
}}
func packed(a bool,p *int,b bool,q *int,c bool,n int)int {
 if n==0 {request<-true;<-done;runtime.Gosched();return *p+*q}
 child:=packed(a,p,b,q,c,n-1)
 if a {child++}
 if b {child+=2}
 if c {child+=4}
 return child+*p+*q
}
func held(a,b,c bool,n int)int {
 local:=&item{n}
 if n==0 {request<-true;<-done;runtime.Gosched();return local.value}
 child:=held(a,b,c,n-1)
 if a {child++}
 if b {child+=2}
 if c {child+=4}
 return child+local.value
}
func nilPointer(){
 defer func(){if recover()==nil {panic("missing pointer panic")}}()
 value:=7
 packed(true,nil,true,&value,true,0)
}
func main(){
 // The nested held call below keeps this controller ordinary. Snapshot
 // expectations before invoking packed so its arguments have no later use.
 go collect()
 rounds:=make(chan int,3)
 rounds<-0;rounds<-1;rounds<-2;close(rounds)
 for round:=range rounds {
  for _,n:=range []int{0,1,64,256,4096} {
   p,q:=new(int),new(int)
   *p,*q=11+round,17+round
   a,b,c:=round&1!=0,round&2!=0,true
   extra:=4
   if a {extra++}
   if b {extra+=2}
   want:=(n+1)*(*p+*q)+n*extra
   order=0
   got:=packed(flag(1,a),pointer(2,p),flag(3,b),pointer(4,q),flag(5,c),depth(6,n))
   if got!=want || order!=123456 {panic("payload or evaluation order")}
   if held(a,b,c,n)!=n*(n+1)/2+n*extra {panic("suspended local pointer")}
   runtime.GC();runtime.GC()
  }
 }
 nilPointer()
 close(request)
 println("self-frame-padding-ok")
}
`
	path := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(path, []byte(source), 0o666); err != nil {
		t.Fatal(err)
	}
	for _, config := range []struct{ name, experiment, flags string }{
		{"inline", "coro", "-d=coro=4"},
		{"noinline", "coro", "-l -d=coro=4 -S"},
		{"checkptr", "coro", "-l -d=coro=4,checkptr=2"},
		{"race", "coro", "-l -d=coro=4"},
		{"disabled", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			if config.name == "race" {
				testenv.MustHaveCGO(t)
			}
			exe := filepath.Join(t.TempDir(), "self-padding.exe")
			args := []string{"build", "-gcflags=" + config.flags, "-o", exe, path}
			if config.name == "race" {
				args = append([]string{"build", "-race"}, args[1:]...)
			}
			cmd := testenv.Command(t, testenv.GoToolPath(t), args...)
			cmd.Env = append(cmd.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			{
				nm := testenv.Command(t, testenv.GoToolPath(t), "tool", "nm", exe)
				symbols, err := nm.CombinedOutput()
				if err != nil {
					t.Fatalf("nm: %v\n%s", err, symbols)
				}
				native := config.experiment == "coro" && config.name != "race"
				for _, name := range []string{"packed", "held"} {
					if got := strings.Contains(string(symbols), "main."+name+".coro"); got != native {
						t.Fatalf("%s native self frame=%v, want %v", name, got, native)
					}
				}
				if strings.Contains(string(symbols), "main.main.coro") {
					t.Fatal("measurement controller unexpectedly lowered")
				}
			}
			if config.name == "noinline" {
				lines := strings.Split(string(out), "\n")
				found := 0
				for i, line := range lines {
					if !strings.HasPrefix(line, "type:noalg.struct {") || !strings.Contains(line, ".coroParent") || strings.Contains(line, ".coroResume") || strings.Contains(line, "*main.item") || strings.Count(line, " bool") != 4 {
						continue
					}
					words := strings.Fields(lines[i+1])
					if len(words) < 9 || words[0] != "0x0000" {
						t.Fatalf("invalid type bytes: %s", lines[i+1])
					}
					var data [8]byte
					for j := range data {
						v, err := strconv.ParseUint(words[j+1], 16, 8)
						if err != nil {
							t.Fatal(err)
						}
						data[j] = byte(v)
					}
					if got := binary.LittleEndian.Uint64(data[:]); got != 80 {
						t.Fatalf("self frame size=%d, want80", got)
					}
					found++
				}
				if found != 1 {
					t.Fatalf("found%d packed self frames, want1", found)
				}
			}
			for _, procs := range []int{1, 4} {
				cmd := testenv.Command(t, exe)
				cmd.Env = append(cmd.Environ(), "GOMAXPROCS="+strconv.Itoa(procs), "GOGC=1")
				out, err := cmd.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "self-frame-padding-ok" {
					t.Fatalf("P%d run: %v\n%s", procs, err, out)
				}
			}
		})
	}
}
