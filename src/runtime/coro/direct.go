// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && ((darwin && arm64) || (linux && amd64))

package coro

// DirectAdd calls the MVP scalar System ABI fixture.
func DirectAdd(a, b uint64) uint64

// DirectBlock calls the MVP blocking System ABI fixture.
func DirectBlock(gate *uint32)
