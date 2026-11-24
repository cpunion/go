// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64 || amd64

package fpprobe

// CaptureHardwareFrames walks the hardware frame pointer chain (R29 on arm64,
// BP on amd64) without invoking Go's coroutine source-traceback rewriter,
// storing up to max return PCs into dst and returning the count captured.
func CaptureHardwareFrames(dst *uintptr, max int) int
