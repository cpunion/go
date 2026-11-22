// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"go/constant"
)

type privateChannelPair struct {
	body      ir.Nodes
	sendIndex int
	recvIndex int
	send      *ir.SendStmt
	receive   *ir.UnaryExpr
	commaOK   *ir.AssignListStmt
}

type privateChannel struct {
	uses  int
	pairs []privateChannelPair
}

// FoldPrivateChannelPairs replaces paired sends and receives on a private,
// buffered channel with a temporary holding the sent value. Only local zero
// declarations may separate a send from its receive. Every channel use must
// belong to such a pair, so each pair starts with an empty buffer and no other
// sender, receiver, or observer. Keep make at its original location.
func FoldPrivateChannelPairs(fn *ir.Func) {
	var channels map[*ir.Name]*privateChannel
	ir.VisitList(fn.Body, func(node ir.Node) {
		if assign, ok := node.(*ir.AssignStmt); ok && assign.Def {
			name, local := assign.X.(*ir.Name)
			makeChan, makeOK := assign.Y.(*ir.MakeExpr)
			if local && name.Class == ir.PAUTO && name.Defn == assign && !name.Addrtaken() &&
				makeOK && makeChan.Op() == ir.OMAKECHAN && ir.IsConst(makeChan.Len, constant.Int) &&
				constant.Sign(makeChan.Len.Val()) > 0 {
				if channels == nil {
					channels = make(map[*ir.Name]*privateChannel)
				}
				// The defining assignment is an allowed use.
				channels[name] = &privateChannel{uses: -1}
			}
		}
	})
	if len(channels) == 0 {
		return
	}

	// Count all occurrences, including captures whose bodies are not children
	// of the closure expression. The declaration itself does not use the
	// channel. Aliases, reassignment, len/cap, selects, and close therefore
	// all make the count exceed the matched operations.
	scan := func(body ir.Nodes) {
		for i := 0; i+1 < len(body); i++ {
			send, ok := body[i].(*ir.SendStmt)
			if !ok || len(send.Init()) != 0 {
				continue
			}
			name, ok := send.Chan.(*ir.Name)
			if !ok || channels[name] == nil {
				continue
			}
			j := i + 1
			for j < len(body) && privateChannelPairDeclaration(body[j]) {
				j++
			}
			if j == len(body) {
				continue
			}
			recv, commaOK := channelReceiveProjection(body[j])
			if recv != nil && recv.X == name {
				ch := channels[name]
				ch.pairs = append(ch.pairs, privateChannelPair{body, i, j, send, recv, commaOK})
			}
		}
	}
	scan(fn.Body)
	ir.VisitList(fn.Body, func(node ir.Node) {
		switch n := node.(type) {
		case *ir.Name:
			if ch := channels[n.Canonical()]; ch != nil {
				ch.uses++
			}
		case *ir.Decl:
			if ch := channels[n.X]; ch != nil {
				ch.uses--
			}
		case *ir.ClosureExpr:
			for _, capture := range n.Func.ClosureVars {
				if ch := channels[capture.Canonical()]; ch != nil {
					ch.uses++
				}
			}
		case *ir.ForStmt:
			scan(n.Body)
		case *ir.RangeStmt:
			scan(n.Body)
		case *ir.IfStmt:
			scan(n.Body)
			scan(n.Else)
		case *ir.BlockStmt:
			scan(n.List)
		case *ir.InlinedCallExpr:
			scan(n.Body)
		case *ir.CaseClause:
			scan(n.Body)
		case *ir.CommClause:
			scan(n.Body)
		}
	})

	// Use declaration order when creating temporaries for reproducible IR.
	ir.WithFunc(fn, func() {
		for _, name := range fn.Dcl {
			ch := channels[name]
			if ch == nil || ch.uses != 2*len(ch.pairs) {
				continue
			}
			for _, pair := range ch.pairs {
				pair.fold(fn)
			}
		}
	})
}

func privateChannelPairDeclaration(stmt ir.Node) bool {
	if channelEntryDeclaration(stmt) {
		return true
	}
	// The noder represents "var x T" as a zero assignment with its
	// declaration in the assignment's init list.
	assign, ok := stmt.(*ir.AssignStmt)
	if !ok || assign.Y != nil || len(assign.Init()) != 1 {
		return false
	}
	decl, ok := assign.Init()[0].(*ir.Decl)
	return ok && decl.X == assign.X && decl.X.Class == ir.PAUTO
}

func channelReceiveProjection(stmt ir.Node) (*ir.UnaryExpr, *ir.AssignListStmt) {
	var value ir.Node
	switch n := stmt.(type) {
	case *ir.UnaryExpr:
		value = n
	case *ir.AssignStmt:
		value = n.Y
	case *ir.ReturnStmt:
		if len(n.Results) == 1 {
			value = n.Results[0]
		}
	case *ir.AssignListStmt:
		// The noder saves a comma-ok receive to temporaries in the
		// first result's init so the outer assignment can convert them.
		if n.Op() == ir.OAS2 && len(n.Rhs) == 2 {
			first := n.Rhs[0]
			for len(first.Init()) == 0 {
				conv, ok := first.(*ir.ConvExpr)
				if !ok {
					return nil, nil
				}
				first = conv.X
			}
			if len(first.Init()) != 1 {
				return nil, nil
			}
			n, _ = first.Init()[0].(*ir.AssignListStmt)
		}
		if n != nil && n.Op() == ir.OAS2RECV && len(n.Lhs) == 2 && len(n.Rhs) == 1 {
			return channelReceiveEntry(n.Rhs[0]), n
		}
	}
	return channelReceiveEntry(value), nil
}

func (pair privateChannelPair) fold(fn *ir.Func) {
	pos := pair.send.Pos()
	value := typecheck.TempAt(pos, fn, pair.send.Chan.Type().Elem())
	save := ir.NewAssignStmt(pos, value, pair.send.Value)
	save.PtrInit().Append(typecheck.Stmt(ir.NewDecl(pos, ir.ODCL, value)))
	pair.body[pair.sendIndex] = typecheck.Stmt(save)

	stmt := pair.body[pair.recvIndex]
	if stmt == pair.receive {
		pair.body[pair.recvIndex] = typecheck.Stmt(ir.NewAssignStmt(stmt.Pos(), ir.BlankNode, value))
		return
	}
	if assign := pair.commaOK; assign != nil {
		assign.SetOp(ir.OAS2)
		assign.Rhs = ir.Nodes{value, ir.NewBool(stmt.Pos(), true)}
		assign.SetTypecheck(0)
		typecheck.Stmt(assign)
		return
	}
	replaceChannelReceive(stmt, pair.receive, value)
}

func replaceChannelReceive(stmt ir.Node, receive *ir.UnaryExpr, value ir.Node) {
	var edit func(ir.Node) ir.Node
	edit = func(node ir.Node) ir.Node {
		if node == receive {
			return value
		}
		ir.EditChildren(node, edit)
		return node
	}
	ir.EditChildren(stmt, edit)
}
