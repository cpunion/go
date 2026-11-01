// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
)

// normalizeReceiveExpressions exposes unconditional single-value receives
// before their enclosing expression. Use the same conservative ordering proof
// as nested awaits: do not cross observable operations or short-circuit arms.
// Loop headers, switch headers, range operands and complex assignments retain
// their ordinary implementation until their evaluation order can be lowered.
func normalizeReceiveExpressions(function *Function) {
	if function == nil || function.Func == nil || function.Primary != CoroPrimary {
		return
	}
	receives := make(map[ir.Node]bool)
	for _, site := range function.Sites {
		if site.Kind == SiteChannel && site.Node != nil && site.Node.Op() == ir.ORECV && site.Node.Type() != nil {
			receives[site.Node] = true
		}
	}
	if len(receives) == 0 {
		return
	}
	fn := function.Func
	oldCurFunc := ir.CurFunc
	ir.CurFunc = fn
	defer func() { ir.CurFunc = oldCurFunc }()
	var list func(ir.Nodes)
	var expression func(ir.Node, bool) (ir.Node, ir.Nodes)
	expression = func(root ir.Node, direct bool) (ir.Node, ir.Nodes) {
		var order []ir.Node
		var visit func(ir.Node)
		visit = func(node ir.Node) {
			if node == nil {
				return
			}
			ir.DoChildren(node, func(child ir.Node) bool { visit(child); return false })
			if receives[node] {
				order = append(order, node)
			}
		}
		visit(root)
		var init ir.Nodes
		for _, recv := range order {
			if direct {
				switch stmt := root.(type) {
				case *ir.UnaryExpr:
					if stmt == recv {
						continue
					}
				case *ir.AssignStmt:
					if stmt.Y == recv {
						continue
					}
				case *ir.AssignListStmt:
					if len(stmt.Rhs) == 1 && stmt.Rhs[0] == recv {
						continue
					}
				}
			}
			if !safeCallPrefix(root, recv) {
				continue
			}
			temp := typecheck.TempAt(recv.Pos(), fn, recv.Type())
			assign := ir.NewAssignStmt(recv.Pos(), temp, recv)
			assign.SetInit(ir.Nodes{ir.NewDecl(recv.Pos(), ir.ODCL, temp)})
			temp.Defn = assign
			init = append(init, typecheck.Stmt(assign))
			var edit func(ir.Node) ir.Node
			edit = func(node ir.Node) ir.Node {
				if node == recv {
					return temp
				}
				ir.EditChildren(node, edit)
				return node
			}
			root = edit(root)
		}
		return root, init
	}
	list = func(nodes ir.Nodes) {
		for _, stmt := range nodes {
			owner, ok := stmt.(ir.InitNode)
			if !ok {
				continue
			}
			prefix := ir.TakeInit(stmt)
			list(prefix)
			var extra ir.Nodes
			switch stmt := stmt.(type) {
			case *ir.BlockStmt:
				list(stmt.List)
			case *ir.IfStmt:
				stmt.Cond, extra = expression(stmt.Cond, false)
				list(stmt.Body)
				list(stmt.Else)
			case *ir.ForStmt:
				list(stmt.Body)
			case *ir.RangeStmt:
				list(stmt.Body)
			case *ir.SwitchStmt:
				for _, clause := range stmt.Cases {
					list(clause.Body)
				}
			case *ir.SelectStmt:
				for _, clause := range stmt.Cases {
					list(clause.Body)
				}
			case *ir.AssignStmt:
				if stmt.X.Op() == ir.ONAME {
					_, extra = expression(stmt, true)
				}
			case *ir.ReturnStmt, *ir.CallExpr:
				_, extra = expression(stmt, true)
			}
			owner.SetInit(append(prefix, extra...))
		}
	}
	list(fn.Body)
}
