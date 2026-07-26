// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

type stacklessCoroG struct {
	stacklessCoro unsafe.Pointer
}

func (gp *g) stackIsFixed() bool {
	return gp.stacklessCoro != nil
}
