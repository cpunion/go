// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.coro || (!darwin && !linux)

package runtime

//go:nowritebarrier
func netpollCoroReadReady(*pollDesc, *int32) uintptr {
	return 0
}

//go:nowritebarrier
func netpollCoroDispatch(*gList, uintptr) {
}
