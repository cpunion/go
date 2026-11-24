// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64

#include "textflag.h"

// func CaptureHardwareFrames(dst *uintptr, max int) int
TEXT ·CaptureHardwareFrames(SB), NOSPLIT, $0-24
	MOVQ	dst+0(FP), DI
	MOVQ	max+8(FP), SI
	XORQ	AX, AX          // count
	MOVQ	BP, BX          // current FP
loop:
	CMPQ	AX, SI
	JGE	done
	TESTQ	BX, BX
	JZ	done
	TESTQ	$7, BX
	JNZ	done
	MOVQ	0(BX), DX       // saved BP
	MOVQ	8(BX), CX       // saved return PC
	TESTQ	CX, CX
	JZ	done
	MOVQ	CX, (DI)
	ADDQ	$8, DI
	INCQ	AX
	CMPQ	DX, BX
	JBE	done
	MOVQ	DX, R8
	SUBQ	BX, R8
	CMPQ	R8, $(256<<10)
	JAE	done
	MOVQ	DX, BX
	JMP	loop
done:
	MOVQ	AX, ret+16(FP)
	RET
