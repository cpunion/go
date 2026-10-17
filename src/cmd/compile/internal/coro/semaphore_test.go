// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
	"strings"
	"testing"
)

func semaphoreTestSignature(pkg *types.Pkg, params int) *types.Type {
	fields := []*types.Field{
		types.NewField(src.NoXPos, pkg.Lookup("addr"), types.NewPtr(types.Types[types.TUINT32])),
		types.NewField(src.NoXPos, pkg.Lookup("flag"), types.Types[types.TBOOL]),
		types.NewField(src.NoXPos, pkg.Lookup("skipframes"), types.Types[types.TINT]),
	}
	return types.NewSignature(nil, fields[:params], nil)
}

var semaphoreTestDeclarations = []struct {
	path, name string
	acquire    bool
	params     int
	operation  string
}{
	{"internal/sync", "runtime_SemacquireMutex", true, 3, "coroSemacquireMutex"},
	{"internal/sync", "runtime_Semrelease", false, 3, "coroSemrelease"},
	{"sync", "runtime_SemacquireRWMutexR", true, 3, "coroSemacquireMutex"},
	{"sync", "runtime_SemacquireRWMutex", true, 3, "coroSemacquireMutex"},
	{"sync", "runtime_Semrelease", false, 3, "coroSemrelease"},
	{"sync", "runtime_Semacquire", true, 1, "coroSemacquire"},
	{"internal/poll", "runtime_Semacquire", true, 1, "coroSemacquire"},
	{"internal/poll", "runtime_Semrelease", false, 1, "coroSemreleasePoll"},
}

func TestSemaphoreRecipeSignature(t *testing.T) {
	prepareLowerTest(t)
	if got := SiteSemaphore.String(); got != "semaphore" {
		t.Fatalf("semaphore site name = %q", got)
	}
	for _, declaration := range semaphoreTestDeclarations {
		pkg := types.NewPkg(declaration.path, "sync")
		name := declaration.name
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), semaphoreTestSignature(pkg, declaration.params))
		recipe, ok := operationRecipe(fn)
		if !ok || recipe.Kind != SiteSemaphore || recipe.Effect != MaySuspend {
			t.Fatalf("%s recipe = %+v, %t, want semaphore suspension", name, recipe, ok)
		}
		for _, typ := range []*types.Type{
			nil,
			types.Types[types.TINT],
			types.NewSignature(nil, nil, nil),
			types.NewSignature(nil, fn.Type().Params(), []*types.Field{
				types.NewField(src.NoXPos, nil, types.Types[types.TBOOL]),
			}),
			types.NewSignature(nil, []*types.Field{
				types.NewField(src.NoXPos, nil, types.NewPtr(types.Types[types.TINT])),
				types.NewField(src.NoXPos, nil, types.Types[types.TBOOL]),
				types.NewField(src.NoXPos, nil, types.Types[types.TINT]),
			}, nil),
		} {
			bad := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), typ)
			recipe, ok := operationRecipe(bad)
			if !ok || recipe.Kind != SiteAwait || recipe.Effect != MaySuspend {
				t.Fatalf("changed %s signature recipe = %+v, %t, want conservative await", name, recipe, ok)
			}
		}
		for index := range declaration.params {
			params := make([]*types.Field, declaration.params)
			for i, field := range fn.Type().Params() {
				typ := field.Type
				if i == index {
					typ = types.Types[types.TUINTPTR]
				}
				params[i] = types.NewField(src.NoXPos, nil, typ)
			}
			bad := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(name), types.NewSignature(nil, params, nil))
			if recipe, ok := operationRecipe(bad); !ok || recipe.Kind != SiteAwait {
				t.Fatalf("changed %s parameter %d recipe = %+v, %t, want conservative await", name, index, recipe, ok)
			}
		}
	}
}

func TestSemaphoreOperation(t *testing.T) {
	prepareLowerTest(t)
	if got := semaphoreOperation(nil); got != "" {
		t.Fatalf("nil semaphore operation = %q", got)
	}
	for _, declaration := range semaphoreTestDeclarations {
		pkg := types.NewPkg(declaration.path, "sync")
		fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup(declaration.name),
			semaphoreTestSignature(pkg, declaration.params))
		call := newLowerTestCall(fn)
		call.Args = make(ir.Nodes, declaration.params)
		if got := semaphoreOperation(call); got != declaration.operation {
			t.Fatalf("%s.%s operation = %q, want %q", declaration.path, declaration.name, got, declaration.operation)
		}
		call.Args = call.Args[:len(call.Args)-1]
		if got := semaphoreOperation(call); got != "" {
			t.Fatalf("changed arity operation = %q", got)
		}
		call.Args = append(call.Args, nil)
		other := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("otherSemaphore"), fn.Type())
		call.Fun = other.Nname
		if got := semaphoreOperation(call); got != "" {
			t.Fatalf("unknown semaphore operation = %q", got)
		}
	}
}

func TestLowerSemaphore(t *testing.T) {
	prepareLowerTest(t)
	oldTarget, oldLocalPkg, oldCurFunc := typecheck.Target, types.LocalPkg, ir.CurFunc
	t.Cleanup(func() {
		typecheck.Target, types.LocalPkg, ir.CurFunc = oldTarget, oldLocalPkg, oldCurFunc
	})
	for _, declaration := range semaphoreTestDeclarations {
		name := declaration.name
		t.Run(declaration.path+"."+name, func(t *testing.T) {
			typecheck.Target = new(ir.Package)
			pkg := types.NewPkg("example.com/coro/semaphore/"+declaration.path+"/"+name, "semaphore")
			types.LocalPkg = pkg
			primitivePkg := types.NewPkg(declaration.path, "sync")
			primitive := ir.NewFunc(src.NoXPos, src.NoXPos, primitivePkg.Lookup(name),
				semaphoreTestSignature(primitivePkg, declaration.params))
			primitive.DeclareParams(true)
			fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("wait"), semaphoreTestSignature(pkg, declaration.params))
			fn.DeclareParams(true)
			typecheck.Target.Funcs = append(typecheck.Target.Funcs, fn)
			call := newLowerTestCall(primitive)
			for _, arg := range fn.Dcl {
				call.Args = append(call.Args, arg)
			}
			if declaration.acquire {
				// An argument initializer belongs to the entry, not the retry.
				counter := typecheck.TempAt(src.NoXPos, fn, types.Types[types.TINT])
				call.SetInit(ir.Nodes{ir.NewDecl(src.NoXPos, ir.ODCL, counter),
					ir.NewAssignStmt(src.NoXPos, counter,
						typedInt(src.NoXPos, types.Types[types.TINT], 17))})
			}
			fn.Body = ir.Nodes{call, newLowerTestReturn()}
			plan, err := Analyze([]*ir.Func{fn}, nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Lower(plan, nil)
			if err != nil || result.Lowered != 1 || result.Skipped != 0 {
				t.Fatalf("Lower = %+v, %v", result, err)
			}
			if plan.Functions[fn].Factory != FactoryABI3 {
				t.Fatalf("factory = %v, want explicit frame", plan.Functions[fn].Factory)
			}
			want := "runtime." + declaration.operation
			found := 0
			var resume *ir.Func
			var helper *ir.CallExpr
			for _, generated := range typecheck.Target.Funcs {
				ir.VisitList(generated.Body, func(node ir.Node) {
					call, ok := node.(*ir.CallExpr)
					if ok && symbolName(ir.StaticCalleeName(ir.StaticValue(call.Fun))) == want {
						found++
						resume, helper = generated, call
					}
				})
			}
			if found != 1 {
				t.Fatalf("generated %d calls to %s, want 1", found, want)
			}
			checkSemaphoreTransition(t, resume, helper, declaration.acquire)
		})
	}
}

func TestLowerSemaphoreChangedCall(t *testing.T) {
	prepareLowerTest(t)
	pkg := types.NewPkg("internal/sync", "sync")
	primitive := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("runtime_SemacquireMutex"), semaphoreTestSignature(pkg, 3))
	fn := ir.NewFunc(src.NoXPos, src.NoXPos, pkg.Lookup("changedCall"),
		semaphoreTestSignature(pkg, 3))
	fn.DeclareParams(true)
	call := newLowerTestCall(primitive)
	for _, arg := range fn.Dcl {
		call.Args = append(call.Args, arg)
	}
	fn.Body = ir.Nodes{call, newLowerTestReturn()}
	plan, err := Analyze([]*ir.Func{fn}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Recheck the target at lowering time, even when inference selected the
	// semaphore recipe. The same ABI on another function is not sufficient.
	other := ir.NewFunc(src.NoXPos, src.NoXPos,
		pkg.Lookup("otherSemaphore"), semaphoreTestSignature(pkg, 3))
	call.Fun = other.Nname
	if _, err := newLowerCandidate(plan, plan.Functions[fn]); err == nil ||
		!strings.Contains(err.Error(), "unsupported semaphore site") {
		t.Fatalf("changed semaphore target error = %v", err)
	}
}

func checkSemaphoreTransition(t *testing.T, resume *ir.Func, helper *ir.CallExpr, acquire bool) {
	t.Helper()
	var label *types.Sym
	params := len(helper.Args) - 2 // context and waiter state are not source arguments.
	for i, node := range resume.Body {
		if node, ok := node.(*ir.LabelStmt); ok {
			label = node.Label
		}
		branch, ok := node.(*ir.IfStmt)
		if !ok || branch.Cond != helper {
			continue
		}
		wantAction := actionHandoff
		if acquire {
			wantAction = actionWait
		}
		ret, ok := branch.Body[0].(*ir.ReturnStmt)
		if !ok || len(ret.Results) != 1 || ir.Int64Val(ret.Results[0]) != int64(wantAction) {
			t.Fatalf("semaphore transition does not return action %d", wantAction)
		}
		if !acquire {
			assignment, ok := resume.Body[i-1].(*ir.AssignStmt)
			if !ok || assignment.X.Type() != types.Types[types.TUINT32] {
				t.Fatal("release does not save its continuation before calling runtime")
			}
			return
		}
		var retryPC int64 = -1
		for _, node := range resume.Body {
			if dispatch, ok := node.(*ir.SwitchStmt); ok {
				for _, entry := range dispatch.Cases {
					if entry.Body[0].(*ir.BranchStmt).Label == label {
						retryPC = ir.Int64Val(entry.List[0])
					}
				}
			}
		}
		for j, node := range resume.Body {
			jump, ok := node.(*ir.BranchStmt)
			if !ok || jump.Label != label || j < params+2 {
				continue
			}
			pc, ok := resume.Body[j-params-1].(*ir.AssignStmt)
			if !ok || pc.X.Type() != types.Types[types.TUINT32] ||
				ir.Int64Val(pc.Y) != retryPC {
				t.Fatal("acquire entry does not save the retry PC")
			}
			entry := resume.Body[j-params-2].(*ir.LabelStmt)
			initializers := 0
			for k, node := range resume.Body {
				init, ok := node.(*ir.AssignStmt)
				if !ok || init.Y.Op() != ir.OLITERAL || ir.Int64Val(init.Y) != 17 {
					continue
				}
				initializers++
				jump, ok := resume.Body[k+1].(*ir.BranchStmt)
				if !ok || jump.Label != entry.Label {
					t.Fatal("argument initializer does not precede the acquire entry")
				}
			}
			if initializers != 1 {
				t.Fatalf("argument initializer occurs %d times, want 1", initializers)
			}
			for k := range params {
				assignment, ok := resume.Body[j-params+k].(*ir.AssignStmt)
				if !ok || !ir.SameSafeExpr(assignment.X, helper.Args[k+1]) {
					t.Fatalf("acquire argument %d is not saved before retry", k)
				}
			}
			return
		}
		t.Fatal("acquire has no separate argument-evaluation entry")
	}
	t.Fatal("missing conditional semaphore transition")
}
