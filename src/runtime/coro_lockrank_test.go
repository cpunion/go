// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && goexperiment.greenteagc && ((darwin && arm64) || (linux && amd64))

package runtime_test

import (
	"runtime"
	"testing"
)

func TestStacklessCoroWriteBarrierLockRanks(t *testing.T) {
	for kind, name := range []string{"scheduler", "registry", "shared", "read-pool", "native-pool"} {
		for _, spans := range []bool{false, true} {
			queue := "wbufSpans"
			if spans {
				queue = "spanSPMCs"
			}
			t.Run(name+"/"+queue, func(t *testing.T) {
				runtime.StacklessCoroWriteBarrierLockForTest(kind, spans)
			})
		}
	}
}
