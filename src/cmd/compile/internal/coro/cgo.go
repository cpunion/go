// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"fmt"
	"strconv"
	"strings"
)

const cgoDirectVersion = "v1"

// maxCgoDirectParams is the widest supported signature: eight integer and
// eight floating-point argument registers.
const maxCgoDirectParams = 16

const (
	maxCgoDirectAggregateSize  = 128
	maxCgoDirectAggregateAlign = 8
)

type cgoABIType uint8

const (
	cgoABIInvalid cgoABIType = iota
	cgoABIInt8
	cgoABIInt16
	cgoABIInt32
	cgoABIInt64
	cgoABIUint8
	cgoABIUint16
	cgoABIUint32
	cgoABIUint64
	cgoABIFloat32
	cgoABIFloat64
	cgoABIPointer
	cgoABIMemory
	cgoABIVoid
)

type cgoDirectCall struct {
	wrapper string
	direct  string
	symbol  string
	class   ForeignCallClass
	params  []cgoABIType
	result  cgoABIType
	errno   bool
}

func (call cgoDirectCall) recipe() OperationRecipe {
	recipe := OperationRecipe{
		Kind:    SiteForeign,
		Foreign: call.class,
		Direct:  call.direct,
		Errno:   call.errno,
	}
	switch call.class {
	case DirectNoBlock:
		recipe.Exec = NeedsSystemABI
	case DirectMayBlock:
		recipe.Exec = NeedsSystemABI | MayBlockThread
	case AsyncOperation:
		recipe.Effect = MaySuspend
		recipe.Exec = NeedsSystemABI
	}
	return recipe
}

func parseCgoDirectives(directives [][]string) (map[string]cgoDirectCall, error) {
	calls := make(map[string]cgoDirectCall)
	for _, directive := range directives {
		if len(directive) != 9 || directive[0] != "cgo_direct" {
			return nil, fmt.Errorf("invalid cgo direct directive %q", directive)
		}
		if directive[1] != cgoDirectVersion {
			return nil, fmt.Errorf("unsupported cgo direct metadata version %q", directive[1])
		}

		call := cgoDirectCall{
			wrapper: directive[2],
			direct:  directive[3],
			symbol:  directive[4],
		}
		if call.wrapper == "" || call.direct == "" || call.symbol == "" {
			return nil, fmt.Errorf("cgo direct directive has an empty symbol")
		}
		if _, ok := calls[call.wrapper]; ok {
			return nil, fmt.Errorf("duplicate cgo direct wrapper %q", call.wrapper)
		}

		switch directive[5] {
		case "mayblock":
			call.class = DirectMayBlock
		default:
			return nil, fmt.Errorf("unsupported cgo direct call class %q", directive[5])
		}

		var err error
		call.params, err = parseCgoABIParams(directive[6])
		if err != nil {
			return nil, err
		}
		call.result, err = parseCgoABIType(directive[7])
		if err != nil {
			return nil, err
		}
		switch directive[8] {
		case "-":
		case "errno":
			call.errno = true
		default:
			return nil, fmt.Errorf("unsupported cgo direct errno policy %q", directive[8])
		}
		calls[call.wrapper] = call
	}
	return calls, nil
}

func parseCgoABIParams(value string) ([]cgoABIType, error) {
	if value == "-" {
		return nil, nil
	}
	fields := strings.Split(value, ",")
	if len(fields) > maxCgoDirectParams {
		return nil, fmt.Errorf("cgo direct call has %d parameters, maximum is %d", len(fields), maxCgoDirectParams)
	}
	params := make([]cgoABIType, len(fields))
	for i, field := range fields {
		var err error
		params[i], err = parseCgoABIType(field)
		if err != nil {
			return nil, err
		}
		if params[i] == cgoABIVoid {
			return nil, fmt.Errorf("cgo direct parameter %d has void type", i)
		}
	}
	return params, nil
}

func parseCgoABIType(value string) (cgoABIType, error) {
	switch value {
	case "i8":
		return cgoABIInt8, nil
	case "i16":
		return cgoABIInt16, nil
	case "i32":
		return cgoABIInt32, nil
	case "i64":
		return cgoABIInt64, nil
	case "u8":
		return cgoABIUint8, nil
	case "u16":
		return cgoABIUint16, nil
	case "u32":
		return cgoABIUint32, nil
	case "u64":
		return cgoABIUint64, nil
	case "f32":
		return cgoABIFloat32, nil
	case "f64":
		return cgoABIFloat64, nil
	case "ptr":
		return cgoABIPointer, nil
	case "void":
		return cgoABIVoid, nil
	default:
		if err := parseCgoABIMemory(value); err == nil {
			return cgoABIMemory, nil
		}
		return cgoABIInvalid, fmt.Errorf("unsupported cgo direct ABI type %q", value)
	}
}

func parseCgoABIMemory(value string) error {
	value, ok := strings.CutPrefix(value, "mem")
	if !ok {
		return fmt.Errorf("missing memory prefix")
	}
	sizeText, alignText, ok := strings.Cut(value, ":")
	if !ok {
		return fmt.Errorf("missing memory alignment")
	}
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil || size <= 0 || size > maxCgoDirectAggregateSize {
		return fmt.Errorf("invalid memory size")
	}
	align, err := strconv.ParseInt(alignText, 10, 64)
	if err != nil || align <= 0 || align > maxCgoDirectAggregateAlign ||
		align&(align-1) != 0 {
		return fmt.Errorf("invalid memory alignment")
	}
	return nil
}
