// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package corosynctest

/*
#cgo noescape add
#cgo nocallback add
static unsigned long long add(unsigned long long a, unsigned long long b) {
	return a + b;
}
*/
import "C"

func foreign(value uint64) uint64 {
	result := C.add(C.ulonglong(value), 7)
	return uint64(result)
}
