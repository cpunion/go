// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && cgo

package suitev2

/*
#include <mach/mach.h>
#include <stdint.h>

static int suitev2_mach_rss(uint64_t *live, uint64_t *peak) {
	mach_task_basic_info_data_t info;
	mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
	kern_return_t kr = task_info(mach_task_self(), MACH_TASK_BASIC_INFO,
		(task_info_t)&info, &count);
	if (kr != KERN_SUCCESS) {
		return -1;
	}
	*live = (uint64_t)info.resident_size;
	*peak = (uint64_t)info.resident_size_max;
	return 0;
}

static uint64_t suitev2_may_callback_noop(uint64_t x) {
	return x + 1;
}
*/
import "C"

func processRSS() (liveBytes, peakBytes uint64) {
	var live, peak C.uint64_t
	if C.suitev2_mach_rss(&live, &peak) == 0 {
		return uint64(live), uint64(peak)
	}
	return 0, 0
}

// ordinaryCgoFallbackOp intentionally omits #cgo nocallback so the coroutine
// compiler keeps it on a growing Go stack (ordinary fallback control).
//
//go:noinline
func ordinaryCgoFallbackOp(x uint64) uint64 {
	return uint64(C.suitev2_may_callback_noop(C.uint64_t(x)))
}
