// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && darwin && arm64

#include "textflag.h"

TEXT ·DirectAdd(SB),NOSPLIT,$0-24
	MOVD	a+0(FP), R0
	MOVD	b+8(FP), R1
	CALL	coro_add_u64(SB)
	MOVD	R0, ret+16(FP)
	RET

TEXT ·DirectBlock(SB),NOSPLIT,$0-8
	MOVD	gate+0(FP), R0
	CALL	coro_block_until(SB)
	RET
