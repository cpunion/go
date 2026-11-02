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

func TestNestedAwaitInRangeAndSelect(t *testing.T) {
	if !(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" || runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("native executor unavailable")
	}
	testenv.MustHaveGoBuild(t)
	const source = `package main
import "runtime"
var calls int
//go:noinline
func awaited(v int) bool {calls++;runtime.Gosched();return v<3}
//go:noinline
func integers()int {sum:=0;for i:=range 5 {if !awaited(i){break};sum+=i};return sum}
//go:noinline
func slices()int {sum:=0;for _,v:=range []int{0,1,2,3,4} {if !awaited(v){break};sum+=v};return sum}
//go:noinline
func arrays()int {sum:=0;for _,v:=range [5]int{0,1,2,3,4} {if !awaited(v){break};sum+=v};return sum}
//go:noinline
func arrayPointers(p *[5]int)int {sum:=0;for _,v:=range p {if !awaited(v){break};sum+=v};return sum}
//go:noinline
func stringsRange()int {sum:=0;for i:=range "abcde" {if !awaited(i){break};sum+=i};return sum}
//go:noinline
func maps()int {sum:=0;for k,v:=range map[int]int{1:10,2:20} {if !awaited(k){panic("map value")};sum+=v};return sum}
//go:noinline
func channels(ch chan int)int {sum:=0;for v:=range ch {if !awaited(v){break};sum+=v};return sum}
//go:noinline
func selected(ch chan int)int {select {case v:=<-ch:if !awaited(v){return -1};return v;default:if v:=1;!awaited(v){return -2};return 1}}
//go:noinline
func nested()int {sum:=0;for i:=range 3 {select {default:if !awaited(i){panic("nested")};sum+=i}};return sum}
//go:noinline
func shortCircuit(flag bool)int {sum:=0;for i:=range 3 {if flag&&awaited(i){sum+=i}};return sum}
//go:noinline
func header()int {sum:=0;for i:=0;awaited(i);i++ {sum+=i};return sum}
var cleaned int
//go:noinline
func failed() (caught bool) {defer func(){cleaned++;caught=recover()!=nil}();f:=awaited;f=nil;for range 1{if !f(0){break}};return false}
func main(){
 if integers()!=3||slices()!=3||arrays()!=3||arrayPointers(&[5]int{0,1,2,3,4})!=3||stringsRange()!=3||maps()!=30 {panic("range body await")}
 ch:=make(chan int,5);for i:=range 5 {ch<-i};close(ch);if channels(ch)!=3 {panic("channel body await")}
 ready:=make(chan int,1);ready<-2;before:=calls;if selected(ready)!=2||calls!=before+1 {panic("selected body exactly once")};before=calls;if selected(ready)!=1||calls!=before+1 {panic("selected default exactly once")}
 if nested()!=3 {panic("nested range/select")};before=calls;if shortCircuit(false)!=0||calls!=before {panic("short circuit hoisted")};if shortCircuit(true)!=3||calls!=before+3 {panic("short circuit result")};if header()!=3 {panic("loop header evaluated once")}
 if !failed()||cleaned!=1 {panic("nested nil call cleanup")};println("range-await-ok")
}
`
	for _, config := range []struct{ name, experiment, flags string }{
		{"native", "coro", "-l -d=coro=4"},
		{"checkptr", "coro", "-l -d=coro=4 -d=checkptr=2"},
		{"native-inline", "coro", "-d=coro=4"},
		{"checkptr-inline", "coro", "-d=coro=4 -d=checkptr=2"},
		{"native-no-dwarf", "coro", "-d=coro=4 -dwarf=false -gendwarfinl=0"},
		{"ordinary", "nocoro", "-l"},
	} {
		t.Run(config.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.go")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "program")
			build := testenv.Command(t, testenv.GoToolPath(t), "build", "-gcflags="+config.flags, "-o", binary, path)
			build.Env = append(build.Environ(), "GOEXPERIMENT="+config.experiment, "GOWORK=off")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, out)
			}
			if config.experiment == "coro" {
				for _, name := range []string{"integers", "slices", "arrays", "arrayPointers", "stringsRange", "maps", "channels", "selected", "nested"} {
					if linkedCoroStackUnsafe(t, binary, "main."+name+".coro.stackgate") {
						t.Fatalf("%s lost native admission", name)
					}
				}
			}
			for _, p := range []string{"1", "4"} {
				run := testenv.Command(t, binary)
				run.Env = append(run.Environ(), "GOMAXPROCS="+p, "GOGC=1", "GODEBUG=asyncpreemptoff=1")
				out, err := run.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != "range-await-ok" {
					t.Fatalf("P%s: %v\n%s", p, err, out)
				}
			}
		})
	}
}
