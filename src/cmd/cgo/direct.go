// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"go/ast"
	"io"
	"strings"

	"internal/buildcfg"
)

const cgoDirectVersion = "v1"

type cgoDirectType string

const (
	cgoDirectInt8    cgoDirectType = "i8"
	cgoDirectInt16   cgoDirectType = "i16"
	cgoDirectInt32   cgoDirectType = "i32"
	cgoDirectInt64   cgoDirectType = "i64"
	cgoDirectUint8   cgoDirectType = "u8"
	cgoDirectUint16  cgoDirectType = "u16"
	cgoDirectUint32  cgoDirectType = "u32"
	cgoDirectUint64  cgoDirectType = "u64"
	cgoDirectPointer cgoDirectType = "ptr"
	cgoDirectVoid    cgoDirectType = "void"
)

type cgoDirectCall struct {
	wrapper string
	direct  string
	symbol  string
	params  []cgoDirectType
	result  cgoDirectType
	errno   bool
}

func (p *Package) directCall(n *Name) (cgoDirectCall, bool) {
	if !buildcfg.Experiment.Coro || *gccgo || n.Kind != "func" ||
		n.FuncType == nil || !p.noCallbacks[n.C] {
		return cgoDirectCall{}, false
	}

	const maxRegisterParams = 6
	if len(n.FuncType.Params) > maxRegisterParams {
		return cgoDirectCall{}, false
	}
	params := make([]cgoDirectType, len(n.FuncType.Params))
	for i, param := range n.FuncType.Params {
		var ok bool
		params[i], ok = cgoDirectTypeOf(param)
		if !ok || params[i] == cgoDirectVoid {
			return cgoDirectCall{}, false
		}
	}

	result := cgoDirectVoid
	if n.FuncType.Result != nil {
		var ok bool
		result, ok = cgoDirectTypeOf(n.FuncType.Result)
		if !ok || result == cgoDirectVoid {
			return cgoDirectCall{}, false
		}
	}

	const wrapperPrefix = "_Cfunc_"
	if !strings.HasPrefix(n.Mangle, wrapperPrefix) {
		return cgoDirectCall{}, false
	}
	direct := "_Cdirect_" + strings.TrimPrefix(n.Mangle, wrapperPrefix)
	return cgoDirectCall{
		wrapper: n.Mangle,
		direct:  direct,
		symbol:  n.C,
		params:  params,
		result:  result,
		errno:   n.AddError,
	}, true
}

func (call cgoDirectCall) writeDirective(w io.Writer) {
	params := "-"
	if len(call.params) != 0 {
		names := make([]string, len(call.params))
		for i, param := range call.params {
			names[i] = string(param)
		}
		params = strings.Join(names, ",")
	}
	errno := "-"
	if call.errno {
		errno = "errno"
	}
	fmt.Fprintf(w, "//go:cgo_direct %s %s %s %s mayblock %s %s %s\n",
		cgoDirectVersion, call.wrapper, call.direct, call.symbol,
		params, call.result, errno)
}

func cgoDirectTypeOf(t *Type) (cgoDirectType, bool) {
	if t == nil {
		return cgoDirectVoid, true
	}
	if t.BadPointer {
		return cgoDirectPointer, true
	}
	return cgoDirectTypeOfExpr(t.Go, t.Size, make(map[string]bool))
}

func cgoDirectTypeOfExpr(expr ast.Expr, size int64, seen map[string]bool) (cgoDirectType, bool) {
	switch expr := expr.(type) {
	case *ast.StarExpr:
		return cgoDirectPointer, true
	case *ast.Ident:
		name := expr.Name
		if strings.HasPrefix(name, "_Ctype_") {
			if seen[name] {
				return "", false
			}
			underlying := typedef[name]
			if underlying == nil {
				return "", false
			}
			seen[name] = true
			return cgoDirectTypeOfExpr(underlying.Go, underlying.Size, seen)
		}
		switch name {
		case "unsafe.Pointer":
			return cgoDirectPointer, true
		case "bool", "byte", "uint8":
			return cgoDirectUint8, size == 1
		case "int8":
			return cgoDirectInt8, size == 1
		case "uint16":
			return cgoDirectUint16, size == 2
		case "int16":
			return cgoDirectInt16, size == 2
		case "uint32":
			return cgoDirectUint32, size == 4
		case "int32":
			return cgoDirectInt32, size == 4
		case "uint64":
			return cgoDirectUint64, size == 8
		case "int64":
			return cgoDirectInt64, size == 8
		case "uint", "uintptr":
			switch size {
			case 4:
				return cgoDirectUint32, true
			case 8:
				return cgoDirectUint64, true
			}
		case "int":
			switch size {
			case 4:
				return cgoDirectInt32, true
			case 8:
				return cgoDirectInt64, true
			}
		}
	}
	return "", false
}
