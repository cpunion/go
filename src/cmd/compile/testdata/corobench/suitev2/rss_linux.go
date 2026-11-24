// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && cgo

package suitev2

/*
#include <stdint.h>

static uint64_t suitev2_may_callback_noop(uint64_t x) {
	return x + 1;
}
*/
import "C"

import (
	"os"
	"strconv"
	"strings"
)

func processRSS() (liveBytes, peakBytes uint64) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			liveBytes = parseProcStatusKB(line) * 1024
		} else if strings.HasPrefix(line, "VmHWM:") {
			peakBytes = parseProcStatusKB(line) * 1024
		}
	}
	return liveBytes, peakBytes
}

func parseProcStatusKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ordinaryCgoFallbackOp intentionally omits #cgo nocallback so the coroutine
// compiler keeps it on a growing Go stack (ordinary fallback control).
//
//go:noinline
func ordinaryCgoFallbackOp(x uint64) uint64 {
	return uint64(C.suitev2_may_callback_noop(C.uint64_t(x)))
}
