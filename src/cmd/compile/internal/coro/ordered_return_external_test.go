// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro_test

import "testing"

const orderedReturnSource = `package main

import "runtime"

var effects int

//go:noinline
func readStep(data *[440]byte) int {
	effects++
	return int(data[7]) + int(data[8])
}

func orderedA(n int, p *int) int {
	var data [440]byte
	data[7] = byte(n)
	if n <= 0 {
		runtime.Gosched()
		*p += 7
		return readStep(&data) + *p
	}
	child := orderedB(n-1, p)
	*p = 3 * *p + n
	return child + readStep(&data) + *p
}

func orderedB(n int, p *int) int {
	var data [440]byte
	data[7] = byte(n)
	if n <= 0 {
		runtime.Gosched()
		*p += 7
		return readStep(&data) + *p
	}
	child := orderedA(n-1, p)
	*p = 3 * *p + n
	return child + readStep(&data) + *p
}

func caller(n int, p *int) int {
	result := orderedA(n, p)
	runtime.Gosched()
	return result
}

func nilValue() (recovered any) {
	defer func() { recovered = recover() }()
	orderedA(40, nil)
	return nil
}

func main() {
	for _, depth := range []int{-1, 0, 1, 2, 255, 256, 257, 512, 513, 4096} {
		for _, alias := range []bool{false, true} {
			effects = 0
			local := 3
			p := &local
			if alias {
				effects = 3
				p = &effects
			}
			got := caller(depth, p)
			wantEffects := 0
			wantValue := 3
			if alias {
				wantEffects = 3
			}
			value := &wantValue
			if alias {
				value = &wantEffects
			}
			*value += 7
			wantEffects++
			want := int(byte(min(depth, 0))) + *value
			for d := 1; d <= depth; d++ {
				*value = 3 * *value + d
				wantEffects++
				want += int(byte(d)) + *value
			}
			if got != want || effects != wantEffects || *p != *value {
				panic("incorrect recursion effects or result")
			}
		}
	}
	effects = 0
	if _, ok := nilValue().(runtime.Error); !ok || effects != 0 {
		panic("incorrect nil fallback")
	}
	runtime.LockOSThread()
	value := 5
	if got := orderedA(1, &value); got != 50 || value != 37 || effects != 2 {
		panic("incorrect locked-thread fallback")
	}
	runtime.UnlockOSThread()
	println("switch-return-ok")
}
`

func TestOrderedReturnEffects(t *testing.T) {
	lowered := []string{"orderedA", "orderedB", "caller"}
	testSwitchReturnFilesNative(t, map[string]string{"main.go": orderedReturnSource}, "", nil, lowered, lowered)
}
