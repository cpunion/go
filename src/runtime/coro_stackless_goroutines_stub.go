// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !goexperiment.coro

package runtime

import (
	"internal/profilerecord"
	"unsafe"
)

func stacklessCoroUnhostedGoroutines() int64                                          { return 0 }
func stacklessCoroInitHostG(*funcval, *g, *g) uint64                                  { return 0 }
func stacklessCoroGoroutineProfile([]profilerecord.StackRecord, []unsafe.Pointer) int { return 0 }
func stacklessCoroTracebackGoroutines()                                               {}
