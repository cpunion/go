// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !race && ((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

func StacklessCoroChannelPopulationForTest(channel any, tokens []unsafe.Pointer) int {
	c := (*hchan)(efaceOf(&channel).data)
	lock(&c.lock)
	n := 0
	for sg := c.recvq.first; sg != nil; sg = sg.next {
		if owner := sg.coro.get(); owner != nil {
			if n < len(tokens) {
				tokens[n] = owner
			}
			n++
		}
	}
	unlock(&c.lock)
	return n
}
