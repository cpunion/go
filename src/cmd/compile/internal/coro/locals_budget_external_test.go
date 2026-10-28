// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestInvocationLocalStorageFallback(t *testing.T) {
	const source = `package main
import "runtime"
func suffix(n int) int {
 runtime.Gosched()
 var data [5120]byte
 for i:=0;i<len(data);i++ {data[i]=byte(i+n)}
 total:=0
 for i:=0;i<len(data);i++ {total+=int(data[i])}
 return total
}
func combined(n int) int {
 runtime.Gosched()
 var a,b [128]byte
 for i:=0;i<128;i++ {a[i]=byte(i+n);b[i]=byte(i+n+1)}
 total:=0
 for i:=0;i<128;i++ {total+=int(a[i])+int(b[i])}
 return total
}
func structure(n int) int {
 runtime.Gosched()
 var data struct{pad [5120]byte}
 for i:=0;i<5120;i++ {data.pad[i]=byte(i+n)}
 total:=0
 for i:=0;i<5120;i++ {total+=int(data.pad[i])}
 return total
}
func small(n int) int {
 runtime.Gosched()
 var data [64]byte
 for i:=0;i<64;i++ {data[i]=byte(i+n)}
 total:=0
 for i:=0;i<64;i++ {total+=int(data[i])}
 return total
}
func boundary(n int) int {
 runtime.Gosched()
 var data [176]byte
 for i:=0;i<176;i++ {data[i]=byte(i+n)}
 total:=0
 for i:=0;i<176;i++ {total+=int(data[i])}
 return total
}
func beyond(n int) int {
 runtime.Gosched()
 var data [184]byte
 for i:=0;i<184;i++ {data[i]=byte(i+n)}
 total:=0
 for i:=0;i<184;i++ {total+=int(data[i])}
 return total
}
func persistent(n int) int {
 var data [5120]byte
 data[0]=byte(n)
 runtime.Gosched()
 total:=0
 for i:=0;i<5120;i++ {total+=int(data[i])}
 return total
}
func main(){for i:=0;i<20;i++ {
 if suffix(i)!=652800||structure(i)!=652800 {panic("large suffix")}
 if combined(i)!=16384+256*i {panic("combined suffix")}
 if small(i)!=2016+64*i {panic("small suffix")}
 if boundary(i)!=15400+176*i||beyond(i)!=16836+184*i {panic("suffix boundary")}
 if persistent(i)!=i {panic("persistent array")}
 runtime.GC()
};println("switch-return-ok")}
`
	testSwitchReturnProgram(t, source, []switchFallback{
		{"suffix", "invocation locals exceed native stack budget"},
		{"combined", "invocation locals exceed native stack budget"},
		{"structure", "invocation locals exceed native stack budget"},
		{"beyond", "invocation locals exceed native stack budget"},
	}, []string{"small", "boundary", "persistent"})
}
