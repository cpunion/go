// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && !((darwin && arm64) || (linux && amd64))

package runtime

import "unsafe"

func stacklessCoroScalarTaskFrameMarker(*stacklessCoroTask, unsafe.Pointer) uint16 { return 0 }
func clearStacklessCoroScalarTaskFrame(task *stacklessCoroTask) {
	task.frameSize = stacklessCoroUncachedFrameLineage
}
func takeStacklessCoroCompletedScalarTaskChunk() *stacklessCoroScalarTaskChunk { return nil }
func cacheStacklessCoroCompletedScalarTaskChunk(*stacklessCoroTask)            {}
