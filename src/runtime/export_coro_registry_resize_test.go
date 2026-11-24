// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

func CheckStacklessCoroRegistryResizeForTest(n int) (grownBuckets, nonEmpty, maxChain, drainedBuckets int, largeNil bool, remaining int64) {
	tasks := make([]stacklessCoroTask, n)
	recs := make([]stacklessCoroGoroutine, n)

	lock(&stacklessCoroGoroutines.lock)
	baseRecords := stacklessCoroGoroutines.records.Load()
	for i := 0; i < n; i++ {
		recs[i] = stacklessCoroGoroutine{root: &tasks[i], leaf: &tasks[i]}
		stacklessCoroInsertGoroutine(&recs[i])
	}
	buckets := stacklessCoroGoroutineBuckets()
	grownBuckets = len(buckets)
	for _, head := range buckets {
		l := 0
		for r := head; r != nil; r = r.next {
			l++
		}
		if l > 0 {
			nonEmpty++
			if l > maxChain {
				maxChain = l
			}
		}
	}
	// Verify every inserted task is findable after all rehashes.
	for i := 0; i < n; i++ {
		if stacklessCoroGoroutineFor(&tasks[i]) != &recs[i] {
			unlock(&stacklessCoroGoroutines.lock)
			return -1, 0, 0, 0, false, -1
		}
	}
	unlock(&stacklessCoroGoroutines.lock)

	for i := 0; i < n; i++ {
		stacklessCoroCompleteLogicalFrame(&tasks[i])
	}

	lock(&stacklessCoroGoroutines.lock)
	drainedBuckets = len(stacklessCoroGoroutineBuckets())
	largeNil = stacklessCoroGoroutines.largeBuckets == nil
	remaining = stacklessCoroGoroutines.records.Load() - baseRecords
	unlock(&stacklessCoroGoroutines.lock)
	return
}
