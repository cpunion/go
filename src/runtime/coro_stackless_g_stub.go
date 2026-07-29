// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.coro

package runtime

type stacklessCoroG struct{}

func (*g) stackIsFixed() bool {
	return false
}

func stacklessCoroRecover(*g) any {
	return nil
}
