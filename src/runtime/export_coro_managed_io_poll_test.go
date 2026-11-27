// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

// StacklessCoroRegisteredIOCountForTest returns the current registered I/O
// operation count tracked for idle-poll short-circuiting.
func StacklessCoroRegisteredIOCountForTest() int32 {
	return stacklessCoroRegisteredIOCount()
}
