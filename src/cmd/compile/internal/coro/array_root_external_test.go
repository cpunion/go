// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

// Check snapshots after parameter mutation, repeated reads, deep serial
// recursion, independent public go factories with nonnil ctx, and GC while
// every lineage is suspended. All variants also run through ordinary Go.
func TestRootArrayLineage(t *testing.T) {
	const source = `package main
import "runtime"
//go:noinline
func read(p *[5120]byte) int { return int(p[0])+int(p[5118])+int(p[5119]) }
func sequence(n,seed int) int {
 var data [5120]byte
 data[5119]=byte(seed)
 seed += 7
 if n==0 {runtime.Gosched();return read(&data)+read(&data)}
 got:=sequence(n-1,seed)
 return got+read(&data)+read(&data)
}
var collect=make(chan bool)
var collected=make(chan bool)
func gcLoop(){for range collect{runtime.GC();collected<-true}}
func independent(n,seed int,done chan int,root bool) int {
 var data [5120]byte
 data[5119]=byte(seed)
 seed+=7
 got:=0
 if n==0 {runtime.Gosched();collect<-true;<-collected} else {got=independent(n-1,seed,done,false)}
 got+=read(&data)+read(&data)
 if root {done<-got}
 return got
}
func spawn(done chan int,n,seed int){go independent(n,seed,done,true);runtime.Gosched()}
func want(n,seed int)int{v:=0;for i:=0;i<=n;i++{v+=2*int(byte(seed+7*i))};return v}
func main(){
 go gcLoop()
 runtime.LockOSThread()
 if sequence(64,19)!=want(64,19){panic("locked-thread ordinary entry")}
 runtime.UnlockOSThread()
 for _,n:=range []int{0,1,16,64,513} {
  for seed:=0;seed<4;seed++ {if sequence(n,seed)!=want(n,seed){panic("serial")}}
  done:=make(chan int,8)
  for seed:=0;seed<8;seed++ {spawn(done,n,seed)}
  total:=0;expected:=0
  for seed:=0;seed<8;seed++ {total+=<-done;expected+=want(n,seed)}
  if total!=expected {panic("independent storage")}
 }
 println("switch-return-ok")
}
`
	testSwitchReturnProgram(t, source, nil, []string{"sequence", "independent"})
}

func TestMutualRootArrayLineage(t *testing.T) {
	testSwitchReturnProgram(t, mutualRootArraySource, nil,
		[]string{"even", "odd", "outside", "differentA", "differentB", "liveA", "liveB", "mixedReadA", "mixedReadB"},
		"even", "odd", "outside", "differentA", "differentB", "liveA", "liveB", "mixedReadA", "mixedReadB")
}

const mutualRootArraySource = `package main

import "runtime"

var ready = make(chan struct{})
var release = make(chan struct{})

//go:noinline
func readPair(p *[440]byte) int { return int(p[0]) + int(p[121]) + 2*int(p[439]) }

func bottom(mode int) {
	switch mode {
	case 1:
		ready <- struct{}{}
		<-release
	case 2:
		panic("array unwind")
	}
	runtime.Gosched()
}

func even(n, seed, mode int) int {
	var data [440]byte
	data[439] = byte(seed)
	seed += 7
	if n == 0 {
		bottom(mode)
		return readPair(&data) + readPair(&data)
	}
	result := odd(n-1, seed, mode)
	return result + readPair(&data) + readPair(&data)
}

func odd(n, seed, mode int) int {
	var data [440]byte
	data[439] = byte(seed)
	seed += 7
	if n == 0 {
		bottom(mode)
		return readPair(&data) + readPair(&data)
	}
	result := even(n-1, seed, mode)
	return result + readPair(&data) + readPair(&data)
}

func outside(n, seed, mode int) int {
	a := even(n, seed, mode)
	b := odd(n, seed+3, mode)
	return a + b
}

func spawn(done chan int, n, seed int) {
	go worker(done, n, seed)
}

func worker(done chan int, n, seed int) {
	done <- outside(n, seed, 1)
}

func differentA(n, seed int) int {
	var data [440]byte
	data[0] = byte(seed)
	if n == 0 {
		runtime.Gosched()
		return readPair(&data)
	}
	child := differentB(n-1, seed+7)
	return child + readPair(&data)
}

func differentB(n, seed int) int {
	var data [440]byte
	data[439] = byte(seed)
	if n == 0 {
		runtime.Gosched()
		return readPair(&data)
	}
	child := differentA(n-1, seed+7)
	return child + readPair(&data)
}

// Only one peer needs its original depth after the await. Keep that live
// input out of the result destination on both paths through the lineage.
func liveA(n int) int {
	var data [440]byte
	data[439] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return readPair(&data)
	}
	child := liveB(n-1)
	return child + n + readPair(&data)
}

func liveB(n int) int {
	var data [440]byte
	data[439] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return readPair(&data)
	}
	child := liveA(n-1)
	return child + readPair(&data)
}

// One peer reads into a local before suspending. Both peers must agree on
// whether their private factories pass a reconstructed buffer.
func mixedReadA(n int) int {
	var data [440]byte
	data[439] = byte(n)
	before := readPair(&data)
	if n == 0 {
		runtime.Gosched()
		return before
	}
	child := mixedReadB(n-1)
	return child + before
}

func mixedReadB(n int) int {
	var data [440]byte
	data[439] = byte(n)
	if n == 0 {
		runtime.Gosched()
		return readPair(&data)
	}
	child := mixedReadA(n-1)
	return child + readPair(&data)
}

func collect() {
	for range ready {
		runtime.GC()
		release <- struct{}{}
	}
}

func want(n, seed int) int {
	total := 0
	for i := 0; i <= n; i++ {
		total += 4 * int(byte(seed+7*i))
	}
	return total
}

func catch(n int) (caught bool) {
	defer func() { caught = recover() == "array unwind" }()
	even(n, 11, 2)
	return false
}

func main() {
	go collect()
	runtime.LockOSThread()
	if outside(13, 251, 0) != want(13, 251)+want(13, 254) {
		panic("locked-thread fallback")
	}
	runtime.UnlockOSThread()
	for _, n := range []int{0, 1, 7, 8, 63, 513} {
		if outside(n, 251, 0) != want(n, 251)+want(n, 254) {
			panic("ordinary caller snapshot")
		}
		if !catch(n) || outside(n, 39, 0) != want(n, 39)+want(n, 42) {
			panic("root reuse after unwind")
		}
		done := make(chan int, 4)
		wantTotal := 0
		for seed := 0; seed < 4; seed++ {
			spawn(done, n, seed)
			wantTotal += want(n, seed) + want(n, seed+3)
		}
		total := 0
		for range 4 { total += <-done }
		if total != wantTotal { panic("concurrent array roots") }
		different := 0
		for i := 0; i <= n; i++ {
			different += (1+i%2)*int(byte(33+7*i))
		}
		if differentA(n, 33) != different { panic("incompatible write indices") }
		wantA, wantB := 0, 0
		for i := 0; i <= n; i++ {
			value := 2 * int(byte(i))
			wantA += value
			wantB += value
			if (n-i)%2 == 0 { wantA += i } else { wantB += i }
		}
		if liveA(n) != wantA || liveB(n) != wantB { panic("live input across await") }
		var wantMixed int
		for i := 0; i <= n; i++ { wantMixed += 2 * int(byte(i)) }
		if mixedReadA(n) != wantMixed || mixedReadB(n) != wantMixed { panic("mixed reader lineage") }
	}
	println("switch-return-ok")
}
`
