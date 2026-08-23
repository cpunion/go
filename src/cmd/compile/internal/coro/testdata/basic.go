// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package basic

var (
	suspend = make(chan struct{})
	before  int64
	after   int64
)

// yieldOnce marks the suspension point recognized by the basic LLVM
// coroutine proof of concept.
//
//go:noinline
func yieldOnce() {
	<-suspend
}

// leaf is deliberately limited to scalar arithmetic around one suspension
// point. The proof of concept checks this shape before handing it to LLVM.
//
//go:noinline
func leaf(x int64) int64 {
	before++
	x++
	yieldOnce()
	after++
	return x + 2
}
