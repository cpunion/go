// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

func TestNormalizeDirectAssignmentStores(t *testing.T) {
	const source = `package main
import "runtime"
type box struct{n int;next *box}
var pointer *box
var trace string
func value()int{trace+="v";runtime.Gosched();return 6}
func redirect()int{trace+="r";runtime.Gosched();pointer=&box{n:100};return 6}
func fail()int{trace+="f";runtime.Gosched();panic("rhs")}
func fieldStore()int{p:=&box{};p.n=value();return p.n}
func indirectStore()int{n:=0;p:=&n;*p=value();return n}
func latePointer(){old:=&box{n:1};pointer=old;pointer.n=redirect();if old.n!=1||pointer.n!=6{panic("early pointer snapshot")}}
func nilStore()(caught any){defer func(){caught=recover()}();var p *box;p.n=value();return}
func nestedNilStore()(caught any){defer func(){caught=recover()}();p:=&box{};p.next.n=value();return}
func rhsPanic()(caught any){defer func(){caught=recover()}();var p *box;p.n=fail();return}
func main(){
 trace="";if fieldStore()!=6||trace!="v"{panic("field result")}
 trace="";if indirectStore()!=6||trace!="v"{panic("pointer result")}
 trace="";latePointer();if trace!="r"{panic("late pointer")}
 trace="";if _,ok:=nilStore().(runtime.Error);!ok||trace!="v"{panic("nil store timing")}
 trace="";if _,ok:=nestedNilStore().(runtime.Error);!ok||trace!="v"{panic("nested nil timing")}
 trace="";if rhsPanic()!="rhs"||trace!="f"{panic("rhs panic priority")}
 println("switch-return-ok")
}
`
	required := []string{"fieldStore", "indirectStore", "latePointer", "nilStore", "nestedNilStore", "rhsPanic"}
	testSwitchReturnProgram(t, source, nil, required, required...)
}
