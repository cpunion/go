// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && darwin && arm64

#include "go_asm.h"
#include "textflag.h"

// func coroNativeGogo(buf *gobuf, newG0 *g)
TEXT runtime·coroNativeGogo(SB), NOSPLIT|NOFRAME, $0-16
	MOVD	buf+0(FP), R5
	MOVD	gobuf_g(R5), R6
	MOVD	0(R6), R4	// make sure g != nil
	MOVD	g_m(R6), R8
	MOVD	newG0+8(FP), R7
	MOVD	R7, m_g0(R8)

	MOVD	R6, g
	BL	runtime·save_g(SB)
	MOVD	gobuf_sp(R5), R0
	MOVD	R0, RSP
	MOVD	gobuf_bp(R5), R29
	MOVD	gobuf_lr(R5), LR
	MOVD	gobuf_ctxt(R5), R26
	MOVD	$0, gobuf_sp(R5)
	MOVD	$0, gobuf_bp(R5)
	MOVD	$0, gobuf_lr(R5)
	MOVD	$0, gobuf_ctxt(R5)
	CMP	ZR, ZR
	MOVD	gobuf_pc(R5), R6
	B	(R6)
