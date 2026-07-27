// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build goexperiment.coro && linux && amd64

#include "textflag.h"

TEXT runtime·coroSubmit(SB),NOSPLIT,$0-28
	MOVQ	id+0(FP), DI
	MOVQ	value+8(FP), SI
	MOVL	fd+16(FP), DX
	CALL	coro_submit_u64(SB)
	MOVL	AX, ret+24(FP)
	RET
