// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

// operationRecipe describes a compiler-owned suspension operation. Recipes
// are selected by the fully qualified identity of a statically resolved Go
// declaration. Target backends consume the resulting OperationKind from a
// frozen SitePlan; they do not repeat this lookup.
type operationRecipe struct {
	kind   OperationKind
	effect Effect
	reason suspendReason
}

var operationRecipes = map[string]operationRecipe{
	"time.Sleep": {
		kind:   OperationTimer,
		effect: MaySuspend,
		reason: timerWait,
	},
}

func lookupOperation(name string) (operationRecipe, bool) {
	recipe, ok := operationRecipes[name]
	return recipe, ok
}
