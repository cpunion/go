// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build arm64

#include "textflag.h"

// func CaptureHardwareFrames(dst *uintptr, max int) int
TEXT ·CaptureHardwareFrames(SB), NOSPLIT, $0-24
	MOVD	dst+0(FP), R0
	MOVD	max+8(FP), R1
	MOVD	$0, R2          // count
	MOVD	R29, R3         // current FP
loop:
	CMP	R1, R2          // R2 - R1
	BGE	done
	CBZ	R3, done
	AND	$7, R3, R6
	CBNZ	R6, done
	MOVD	0(R3), R4       // saved FP
	MOVD	8(R3), R5       // saved LR
	CBZ	R5, done
	MOVD	R5, (R0)
	ADD	$8, R0, R0
	ADD	$1, R2, R2
	CMP	R3, R4          // R4 - R3
	BLS	done
	SUB	R3, R4, R6      // R6 = R4 - R3
	MOVD	$(256<<10), R7
	CMP	R7, R6          // R6 - R7
	BHS	done
	MOVD	R4, R3
	B	loop
done:
	MOVD	R2, ret+16(FP)
	RET
