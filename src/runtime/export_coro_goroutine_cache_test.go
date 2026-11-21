// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

func StacklessCoroFillRecordCacheForTest() (unsafe.Pointer, uint32) {
	lock(&stacklessCoroGoroutines.lock)
	first := new(stacklessCoroGoroutine)
	cacheStacklessCoroRecordLocked(first)
	for range stacklessCoroRecordCache.capacity {
		cacheStacklessCoroRecordLocked(new(stacklessCoroGoroutine))
	}
	unlock(&stacklessCoroGoroutines.lock)
	return unsafe.Pointer(first), stacklessCoroRecordCache.capacity
}

func StacklessCoroRecordCacheCountsForTest() (young, old uint32) {
	lock(&stacklessCoroGoroutines.lock)
	young = stacklessCoroRecordCache.generations[stacklessCoroRecordCache.young].count
	old = stacklessCoroRecordCache.generations[stacklessCoroRecordCache.young^1].count
	unlock(&stacklessCoroGoroutines.lock)
	return
}

func StacklessCoroCheckBucketResizeForTest() bool {
	const initial = 2 * stacklessCoroInlineBuckets
	all := make([]*stacklessCoroGoroutine, 2*initial)
	buckets := all[:initial]
	records := make([]stacklessCoroGoroutine, 2*initial)
	tasks := make([]stacklessCoroTask, len(records))
	for i := range records {
		r := &records[i]
		r.root = &tasks[i]
		index := stacklessCoroGoroutineIndex(r.root, len(buckets))
		r.next, buckets[index] = buckets[index], r
	}
	check := func(buckets []*stacklessCoroGoroutine) bool {
		count := 0
		for i, head := range buckets {
			for r := head; r != nil; r = r.next {
				count++
				if count > len(records) || stacklessCoroGoroutineIndex(r.root, len(buckets)) != uintptr(i) {
					return false
				}
			}
		}
		return count == len(records)
	}
	for range 3 {
		stacklessCoroResizeLargeBuckets(buckets, all)
		if !check(all) {
			return false
		}
		stacklessCoroResizeLargeBuckets(all, buckets)
		if !check(buckets) {
			return false
		}
		for _, head := range all[len(buckets):] {
			if head != nil {
				return false
			}
		}
	}
	return true
}
