// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro

package runtime

import "unsafe"

const (
	stacklessCoroRecordCacheBudget    = 12 << 20
	stacklessCoroRecordCacheGroups    = 2048
	stacklessCoroRecordCacheGroupSize = 64
)

// The registry lock protects only cleared records here. Group roots bound
// the length of each pointer chain and the work to retire a GC generation.
var stacklessCoroRecordCache struct {
	generations [2]stacklessCoroRecordGeneration
	young       uint8
	capacity    uint32
}

type stacklessCoroRecordGeneration struct {
	heads [stacklessCoroRecordCacheGroups]*stacklessCoroGoroutine
	count uint32
}

func init() {
	charge := roundupsize(unsafe.Sizeof(stacklessCoroGoroutine{}), false)
	budget := stacklessCoroRecordCacheBudget - unsafe.Sizeof(stacklessCoroRecordCache)
	stacklessCoroRecordCache.capacity = uint32(min(budget/charge, stacklessCoroRecordCacheGroups*stacklessCoroRecordCacheGroupSize))
}

func takeStacklessCoroCachedRecordLocked() *stacklessCoroGoroutine {
	for _, age := range [2]uint8{1, 0} {
		cache := &stacklessCoroRecordCache.generations[stacklessCoroRecordCache.young^age]
		if cache.count != 0 {
			index := (cache.count - 1) / stacklessCoroRecordCacheGroupSize
			record := cache.heads[index]
			cache.heads[index] = record.next
			cache.count--
			return record
		}
	}
	return nil
}

func cacheStacklessCoroRecordLocked(r *stacklessCoroGoroutine) {
	young := &stacklessCoroRecordCache.generations[stacklessCoroRecordCache.young]
	old := &stacklessCoroRecordCache.generations[stacklessCoroRecordCache.young^1]
	if young.count+old.count >= stacklessCoroRecordCache.capacity {
		return
	}
	index := young.count / stacklessCoroRecordCacheGroupSize
	r.next = young.heads[index]
	young.heads[index] = r
	young.count++
}

// clearpools runs with the world stopped. Inactive records keep no user
// pointers and expire after sitting idle through a full GC cycle.
func clearStacklessCoroGoroutineCache() {
	old := &stacklessCoroRecordCache.generations[stacklessCoroRecordCache.young^1]
	groups := (old.count + stacklessCoroRecordCacheGroupSize - 1) / stacklessCoroRecordCacheGroupSize
	for i := uint32(0); i < groups; i++ {
		old.heads[i].next = nil
		old.heads[i] = nil
	}
	old.count = 0
	stacklessCoroRecordCache.young ^= 1
	if len(stacklessCoroGoroutines.largeBuckets) == 0 {
		if stacklessCoroGoroutines.bucketsIdle {
			stacklessCoroGoroutines.largeBuckets = nil
		}
		stacklessCoroGoroutines.bucketsIdle = true
	}
}
