// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !coropullcompare

package runtime

// stacklessCoroIsPullComparison is constant in ordinary builds, allowing the
// compiler to remove the measurement-only queue policy from the hot path.
func stacklessCoroIsPullComparison(*stacklessCoroScheduler) bool {
	return false
}
