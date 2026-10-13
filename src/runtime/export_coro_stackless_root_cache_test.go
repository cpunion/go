// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && (darwin || linux)

package runtime

import "unsafe"

func CheckStacklessCoroRootCacheOwnershipForTest() bool {
	s := new(stacklessCoroScheduler)
	s.root.setFlag(stacklessCoroTaskCacheFrame, true)
	s.root.frameSize = 16
	s.root.context.frame = unsafe.Pointer(new([16]byte))
	child := &stacklessCoroTask{frameSize: 8}
	child.setFlag(stacklessCoroTaskCacheFrame, true)
	child.context.frame = unsafe.Pointer(new([8]byte))
	s.cachedFrameTasks = 1
	s.cachedFrameBytes = 8
	s.discardCachedFrameTaskLocked(&s.root)
	if s.cachedFrameTasks != 1 || s.cachedFrameBytes != 8 ||
		s.root.hasFlag(stacklessCoroTaskCacheFrame) ||
		s.root.context.frame != nil || s.root.frameSize != 0 {
		return false
	}
	s.discardCachedFrameTaskLocked(child)
	return s.cachedFrameTasks == 0 && s.cachedFrameBytes == 0 &&
		!child.hasFlag(stacklessCoroTaskCacheFrame) &&
		child.context.frame == nil && child.frameSize == 0
}
