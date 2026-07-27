// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && darwin && arm64

#include "textflag.h"

TEXT runtime·coroSubmit(SB),NOSPLIT,$0-28
	MOVD	id+0(FP), R0
	MOVD	value+8(FP), R1
	MOVW	fd+16(FP), R2
	CALL	coro_submit_u64(SB)
	MOVW	R0, ret+24(FP)
	RET
