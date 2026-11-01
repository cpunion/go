// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestOrdinaryHelperGrowingStack(t *testing.T) {
	testSwitchReturnFiles(t, map[string]string{
		"go.mod": "module example.com/ordinarystack\n\ngo 1.28\n",
		"helper/helper.go": `package helper
type Wide [4096]uint64
//go:noinline
func consume(p *Wide) uint64 {return p[0]+p[4095]}
//go:noinline
func Large(v any) uint64 {value:=v.(Wide);return consume(&value)}
//go:noinline
func Local(v byte) uint64 {var data [8192]byte;data[8191]=v;return consumeLocal(&data)}
//go:noinline
func consumeLocal(p *[8192]byte) uint64 {return uint64(p[8191])}
func Cleanup(v any) {if Large(v)!=3 {panic("cleanup value")}}
func Small(v uint64) uint64 {return v+1}
`,
		"main.go": `package main
import("runtime";"example.com/ordinarystack/helper")
//go:noinline
func localHelper(v any) uint64 {value:=v.(helper.Wide);return consume(&value)}
//go:noinline
func consume(p *helper.Wide) uint64 {return p[0]+p[4095]}
func local(c chan any) uint64 {v:=<-c;return localHelper(v)}
func imported(c chan any) uint64 {v:=<-c;return helper.Large(v)}
func alias(c chan any) uint64 {f:=helper.Large;v:=<-c;return f(v)}
func array(c chan byte) uint64 {v:=<-c;return helper.Local(v)}
func cleanup(v any) int {defer helper.Cleanup(v);runtime.Gosched();return 1}
func caught(c chan any) (ok bool) {defer func(){ok=recover()!=nil}();v:=<-c;helper.Large(v);return}
func bounded(c chan uint64) uint64 {v:=<-c;return helper.Small(v)}
func main() {
 for range 8 {
  c:=make(chan any,3);for range 3 {c<-helper.Wide{0:1,4095:2}}
  runtime.GC()
  if local(c)!=3 || imported(c)!=3 || alias(c)!=3 || cleanup(helper.Wide{0:1,4095:2})!=1 {panic("large helper")}
  b:=make(chan byte,1);b<-7;if array(b)!=7 {panic("large local")}
  c<-"wrong";if !caught(c) {panic("lost assertion panic")}
  u:=make(chan uint64,1);u<-9;if bounded(u)!=10 {panic("small helper")}
 }
 println("switch-return-ok")
}
`,
	}, "example.com/ordinarystack/...=", []switchFallback{
		{"local", "ordinary helper requires growing Go stack"},
		{"imported", "ordinary helper requires growing Go stack"},
		{"alias", "ordinary helper requires growing Go stack"},
		{"array", "ordinary helper requires growing Go stack"},
		{"cleanup", "ordinary helper requires growing Go stack"},
		{"caught", "ordinary helper requires growing Go stack"},
	}, []string{"bounded"})
}
