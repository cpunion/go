// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && linux && amd64

#include "textflag.h"

TEXT ·DirectAdd(SB),NOSPLIT,$0-24
	MOVQ	a+0(FP), DI
	MOVQ	b+8(FP), SI
	MOVQ	SP, R12		// callee-saved, preserved across the CALL
	ANDQ	$~15, SP	// alignment for the System V ABI
	CALL	coro_add_u64(SB)
	MOVQ	R12, SP
	MOVQ	AX, ret+16(FP)
	RET

TEXT ·DirectBlock(SB),NOSPLIT,$0-8
	MOVQ	gate+0(FP), DI
	MOVQ	SP, R12		// callee-saved, preserved across the CALL
	ANDQ	$~15, SP	// alignment for the System V ABI
	CALL	coro_block_until(SB)
	MOVQ	R12, SP
	RET
