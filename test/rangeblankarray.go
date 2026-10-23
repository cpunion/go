// run

// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A trailing blank iteration variable must not cause evaluation of a
// constant-length array expression that a single-variable range skips.
package main

var calls int

func array() [3]int {
	calls++
	return [3]int{1, 2, 3}
}

func pointer() *[3]int {
	calls++
	return nil
}

func main() {
	var p *[3]int
	sum := 0
	for i, _ := range *p {
		sum += i
	}
	for _, _ = range *p {
		sum++
	}
	i := 0
	for i, _ = range *p {
		sum += i
	}
	if sum != 9 {
		panic("blank array range evaluated its expression")
	}
	for i, _ := range array() {
		sum += i
	}
	for _, _ = range pointer() {
		sum++
	}
	if calls != 2 || sum != 15 {
		panic("blank array range lost call evaluation")
	}
	ch := make(chan *[3]int, 1)
	ch <- nil
	for i, _ := range <-ch {
		sum += i
	}
	if len(ch) != 0 || sum != 18 {
		panic("blank array range lost receive evaluation")
	}
}
