// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && linux && amd64

#include "go_asm.h"
#include "go_tls.h"
#include "textflag.h"

// func coroNativeGogo(buf *gobuf, newG0 *g)
TEXT runtime·coroNativeGogo(SB), NOSPLIT|NOFRAME, $0-16
	MOVQ	buf+0(FP), BX
	MOVQ	gobuf_g(BX), DX
	MOVQ	0(DX), CX	// make sure g != nil
	MOVQ	g_m(DX), SI
	MOVQ	newG0+8(FP), AX
	MOVQ	AX, m_g0(SI)

	get_tls(CX)
	MOVQ	DX, g(CX)
	MOVQ	DX, R14
	MOVQ	gobuf_sp(BX), SP
	MOVQ	gobuf_ctxt(BX), DX
	MOVQ	gobuf_bp(BX), BP
	MOVQ	$0, gobuf_sp(BX)
	MOVQ	$0, gobuf_ctxt(BX)
	MOVQ	$0, gobuf_bp(BX)
	MOVQ	gobuf_pc(BX), BX
	JMP	BX
