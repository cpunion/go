// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && coropullcompare

package runtime

// stacklessCoroPullComparisonMarker is a test-driver sentinel. A real queue
// tail can never refer to it. The comparison reuses head for its current
// structured leaf, which keeps the production scheduler in its original size
// class.
var stacklessCoroPullComparisonMarker stacklessCoroTask

func stacklessCoroIsPullComparison(s *stacklessCoroScheduler) bool {
	return s.tail == &stacklessCoroPullComparisonMarker
}
