// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !arm64 && !amd64

package fpprobe

func CaptureHardwareFrames(dst *uintptr, max int) int {
	return 0
}
