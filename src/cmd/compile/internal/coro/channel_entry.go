// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package coro

import (
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
)

// installBufferedChannelExchangeEntry attempts the two channel operations
// before creating a root. The ordinary path and a successful exchange both
// produce raw results for the original receive projection and return.
func installBufferedChannelExchangeEntry(candidate *lowerCandidate, anchors ...*ir.Name) bool {
	fn, ordinary := candidate.function.Func, candidate.ordinary
	if candidate.factoryABI != FactoryABI3 || len(candidate.function.Sites) != 2 {
		return false
	}
	first := 0
	for first < len(ordinary.Body) && privateChannelPairDeclaration(ordinary.Body[first]) {
		first++
	}
	if len(ordinary.Body)-first != 3 {
		return false
	}
	send, ok := ordinary.Body[first].(*ir.SendStmt)
	if !ok || len(send.Init()) != 0 ||
		!stableChannelEntryOperand(send.Chan) || !stableChannelEntryOperand(send.Value) ||
		send.Chan.Type().Elem().Size() > 16 {
		return false
	}
	projection := ordinary.Body[first+1]
	if !localChannelExchangeProjection(projection) {
		return false
	}
	receive, commaOK := channelReceiveProjection(projection)
	if receive == nil || receive.X != send.Chan {
		return false
	}
	ret, ok := ordinary.Body[first+2].(*ir.ReturnStmt)
	if !ok || len(ret.Init()) != 0 {
		return false
	}
	for _, result := range ret.Results {
		if !shortChannelPairExpr(result, 0) {
			return false
		}
	}

	native := fn.Body
	remapOrdinaryBody(fn, ordinary)
	ir.WithFunc(fn, func() {
		pos := send.Pos()
		elem := send.Chan.Type().Elem()
		source := typecheck.TempAt(pos, fn, elem)
		result := typecheck.TempAt(pos, fn, elem)
		body := ordinary.Body[:first:first]
		body.Append(
			ir.NewDecl(pos, ir.ODCL, source),
			ir.NewDecl(pos, ir.ODCL, result),
			ir.NewAssignStmt(pos, source, send.Value),
			ir.NewAssignStmt(pos, result, ir.NewZero(pos, elem)),
		)
		send.Value = source
		recv := ir.NewUnaryExpr(pos, ir.ORECV, send.Chan)
		var blocking, receivedPointer ir.Node
		if commaOK == nil {
			blocking = ir.NewAssignStmt(pos, result, recv)
			receivedPointer = typecheck.NodNil()
			replaceChannelReceive(projection, receive, result)
		} else {
			boolType := types.Types[types.TBOOL]
			received := typecheck.TempAt(pos, fn, boolType)
			body.Append(ir.NewDecl(pos, ir.ODCL, received),
				ir.NewAssignStmt(pos, received, ir.NewZero(pos, boolType)))
			receivedPointer = typecheck.NodAddr(received)
			blocking = ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{result, received}, ir.Nodes{recv})
			var projectedOK ir.Node = received
			if target := commaOK.Lhs[1]; !ir.IsBlank(target) && !types.Identical(target.Type(), boolType) {
				projectedOK = typecheck.Conv(received, target.Type())
			}
			commaOK.SetOp(ir.OAS2)
			commaOK.Rhs = ir.Nodes{result, projectedOK}
			commaOK.SetTypecheck(0)
			typecheck.Stmt(commaOK)
		}
		exchanged := typecheck.Call(pos, typecheck.LookupRuntime("coroTryChanExchange", elem, elem, elem),
			ir.Nodes{typecheck.NodNil(), send.Chan, typecheck.NodAddr(source), typecheck.NodAddr(result), receivedPointer}, false)
		miss := ir.NewIfStmt(pos, ordinaryEntryCondition(fn, anchors), ir.Nodes{send, blocking}, native)
		body.Append(ir.NewIfStmt(pos, ir.NewUnaryExpr(pos, ir.ONOT, exchanged), ir.Nodes{miss}, nil),
			projection, ret)
		fn.Body = body
		typecheck.Stmts(fn.Body)
	})
	return true
}

func localChannelExchangeProjection(stmt ir.Node) bool {
	var targets []ir.Node
	switch n := stmt.(type) {
	case *ir.AssignStmt:
		targets = []ir.Node{n.X}
	case *ir.AssignListStmt:
		if len(n.Lhs) != 2 || n.Op() != ir.OAS2 && n.Op() != ir.OAS2RECV {
			return false
		}
		targets = n.Lhs
	default:
		return false
	}
	for _, target := range targets {
		if ir.IsBlank(target) {
			continue
		}
		name, ok := target.(*ir.Name)
		if !ok || name.Class != ir.PAUTO || name.IsClosureVar() || name.Addrtaken() {
			return false
		}
	}
	for _, init := range stmt.Init() {
		if !channelEntryDeclaration(init) {
			return false
		}
	}
	return true
}

// channelEntry is the first channel operation of a public explicit-frame entry.
// On a miss, a private resume state can take the already evaluated operands.
// Factory callers continue to enter the original initial state.
type channelEntry struct {
	first      ir.Node
	firstIndex int
	operation  ir.Node
	initOwner  ir.Node
	receive    *ir.UnaryExpr
	waitState  int
	freeze     bool
	channel    *ir.Name
	value      *ir.Name

	frameChannel *ir.Name
	frameValue   *ir.Name
}

func planChannelEntry(candidate *lowerCandidate, states []lowerState) *channelEntry {
	ordinary := candidate.ordinary
	fn := candidate.function.Func
	if candidate.factoryABI != FactoryABI3 || ordinary == nil || len(ordinary.Body) == 0 ||
		len(candidate.defers) != 0 || candidate.dynamicDefers || candidate.rangeOwner != nil ||
		candidate.rangeBody != nil || candidate.rootArray != nil || candidate.affineRoot != nil ||
		len(candidate.channels) != 1 {
		return nil
	}
	// The native entry can skip operand evaluation without having to copy
	// public stack cells that the compiler lowered into a different layout.
	for _, names := range [][]*ir.Name{fn.Dcl, ordinary.Dcl} {
		for _, name := range names {
			if name.Addrtaken() {
				return nil
			}
		}
	}
	// The public entry already owns an ordinary goroutine. A leading
	// Gosched can yield that goroutine before it evaluates channel operands.
	// Only declarations and these yields may precede the channel operation.
	firstIndex, yields := 0, 0
	for _, stmt := range ordinary.Body {
		if !channelEntryDeclaration(stmt) {
			if sourceGosched(stmt) == nil {
				break
			}
			yields++
		}
		firstIndex++
	}
	if firstIndex == len(ordinary.Body) || len(candidate.function.Sites) != yields+1 {
		return nil
	}
	first := ordinary.Body[firstIndex]
	operation := first
	var initOwner ir.Node
	scanInit := func(node ir.Node) bool {
		for _, init := range node.Init() {
			if channelEntryDeclaration(init) {
				continue
			}
			if operation != first {
				return false
			}
			operation, initOwner = init, node
		}
		return true
	}
	if !scanInit(first) {
		return nil
	}
	// A normalized comma-ok receive can live in the first result projection's
	// Init instead of the containing assignment's Init.
	if assign, ok := first.(*ir.AssignListStmt); ok && len(assign.Rhs) > 1 {
		for _, value := range assign.Rhs {
			for {
				if !scanInit(value) {
					return nil
				}
				conv, ok := value.(*ir.ConvExpr)
				if !ok {
					break
				}
				value = conv.X
			}
		}
	}
	if operation != first {
		for _, init := range operation.Init() {
			if !channelEntryDeclaration(init) {
				return nil
			}
		}
	}

	entry := &channelEntry{first: first, firstIndex: firstIndex, operation: operation, initOwner: initOwner}
	var channel, send ir.Node
	switch stmt := operation.(type) {
	case *ir.SendStmt:
		channel, send = stmt.Chan, stmt.Value
	case *ir.UnaryExpr:
		entry.receive = channelReceiveEntry(stmt)
	case *ir.AssignStmt:
		if stmt.X.Op() != ir.ONAME {
			return nil
		}
		entry.receive = channelReceiveEntry(stmt.Y)
	case *ir.AssignListStmt:
		if (stmt.Op() != ir.OAS2RECV && stmt.Op() != ir.OAS2) || len(stmt.Lhs) != 2 || len(stmt.Rhs) != 1 ||
			stmt.Lhs[0].Op() != ir.ONAME || stmt.Lhs[1].Op() != ir.ONAME {
			return nil
		}
		entry.receive = channelReceiveEntry(stmt.Rhs[0])
	case *ir.ReturnStmt:
		if len(stmt.Results) == 0 {
			return nil
		}
		entry.receive = channelReceiveEntry(stmt.Results[0])
	}
	if entry.receive != nil {
		channel = entry.receive.X
	}
	if channel == nil || operation != first &&
		!normalizedReceiveAssignment(entry.receive, operation, first) {
		return nil
	}

	// All earlier native states correspond to the declarations, yields
	// and operand evaluations already performed by the public entry.
	for i := range states {
		op := states[i].channel
		if op == nil || op.rangeStmt != nil || (op.sendValue == nil) != (send == nil) {
			continue
		}
		entry.waitState = i
		entry.freeze = yields != 0 || !stableChannelEntryOperand(channel) ||
			send != nil && !stableChannelEntryOperand(send)
		entry.channel = typecheck.TempAt(first.Pos(), fn, channel.Type())
		if send != nil || entry.receive != operation {
			entry.value = typecheck.TempAt(first.Pos(), fn, channel.Type().Elem())
		}
		return entry
	}
	return nil
}

func channelEntryDeclaration(stmt ir.Node) bool {
	if len(stmt.Init()) != 0 {
		return false
	}
	if stmt.Op() == ir.ODCL {
		return true
	}
	if assign, ok := stmt.(*ir.AssignStmt); ok && assign.Y == nil {
		if name, ok := assign.X.(*ir.Name); ok {
			return name.Class == ir.PAUTO
		}
	}
	return false
}

// A conversion can use the received value before returning it. No other
// evaluated operand may precede the receive inside the statement.
func channelReceiveEntry(expr ir.Node) *ir.UnaryExpr {
	for expr != nil && len(expr.Init()) == 0 {
		switch n := expr.(type) {
		case *ir.UnaryExpr:
			if n.Op() == ir.ORECV {
				return n
			}
			return nil
		case *ir.ConvExpr:
			switch n.Op() {
			case ir.OCONV, ir.OCONVNOP, ir.OCONVIFACE:
				expr = n.X
				continue
			}
		}
		break
	}
	return nil
}

func stableChannelEntryOperand(expr ir.Node) bool {
	if len(expr.Init()) != 0 {
		return false
	}
	switch n := expr.(type) {
	case *ir.Name:
		return n.Class == ir.PPARAM && !n.Addrtaken()
	case *ir.BasicLit, *ir.NilExpr:
		return true
	case *ir.CompLitExpr:
		return len(n.List) == 0 && (n.Op() == ir.OSTRUCTLIT || n.Op() == ir.OARRAYLIT)
	}
	return false
}

func installChannelEntry(candidate *lowerCandidate, anchors ...*ir.Name) {
	entry := candidate.channelEntry
	fn, ordinary := candidate.function.Func, candidate.ordinary
	native := fn.Body
	remapOrdinaryBody(fn, ordinary)
	ir.WithFunc(fn, func() {
		first, operation := entry.first, entry.operation
		pos := operation.Pos()
		body := ordinary.Body[:entry.firstIndex:entry.firstIndex]
		appendInit := func(node ir.Node) {
			for _, init := range ir.TakeInit(node) {
				if init != operation {
					body.Append(init)
				} else {
					body.Append(ir.TakeInit(operation)...)
				}
			}
		}
		appendInit(first)
		if owner := entry.initOwner; owner != nil && owner != first {
			appendInit(owner)
		}
		var selected ir.Node
		var blocking ir.Node
		var projection ir.Node
		elem := entry.channel.Type().Elem()
		if send, ok := operation.(*ir.SendStmt); ok {
			body.Append(ir.NewAssignStmt(pos, entry.channel, send.Chan),
				ir.NewAssignStmt(pos, entry.value, send.Value))
			selected = typecheck.Call(pos, typecheck.LookupRuntime("coroEntryChanSend", elem, elem),
				ir.Nodes{entry.channel, typecheck.NodAddr(entry.value)}, false)
			blocking = ir.NewSendStmt(pos, entry.channel, entry.value)
		} else {
			body.Append(ir.NewAssignStmt(pos, entry.channel, entry.receive.X))
			pointer := ir.Node(typecheck.NodNil())
			if entry.value != nil {
				pointer = typecheck.NodAddr(entry.value)
			}
			ready := typecheck.TempAt(pos, fn, types.Types[types.TBOOL])
			received := ir.Node(ir.BlankNode)
			recv := ir.NewUnaryExpr(pos, ir.ORECV, entry.channel)
			switch stmt := operation.(type) {
			case *ir.AssignListStmt:
				received = typecheck.TempAt(pos, fn, types.Types[types.TBOOL])
				blocking = ir.NewAssignListStmt(pos, ir.OAS2,
					ir.Nodes{entry.value, received}, ir.Nodes{recv})
				ok := received
				if !ir.IsBlank(stmt.Lhs[1]) {
					ok = typecheck.Conv(ok, stmt.Lhs[1].Type())
				}
				projection = ir.NewAssignListStmt(pos, ir.OAS2, stmt.Lhs, ir.Nodes{entry.value, ok})
			case *ir.UnaryExpr:
				blocking = recv
			default:
				blocking = ir.NewAssignStmt(pos, entry.value, recv)
				var edit func(ir.Node) ir.Node
				edit = func(n ir.Node) ir.Node {
					if n == entry.receive {
						return entry.value
					}
					ir.EditChildren(n, edit)
					return n
				}
				ir.EditChildren(operation, edit)
				projection = operation
			}
			call := typecheck.Call(pos, typecheck.LookupRuntime("coroEntryChanRecv", elem, elem),
				ir.Nodes{pointer, entry.channel}, false)
			body.Append(ir.NewAssignListStmt(pos, ir.OAS2, ir.Nodes{ready, received}, ir.Nodes{call}))
			selected = ready
		}
		miss := ir.NewIfStmt(pos, ordinaryEntryCondition(fn, anchors), ir.Nodes{blocking}, native)
		body.Append(ir.NewIfStmt(pos, ir.NewUnaryExpr(pos, ir.ONOT, selected), ir.Nodes{miss}, nil))
		if projection != nil {
			body.Append(projection)
		}
		if operation != first {
			body.Append(first)
		}
		fn.Body = append(body, ordinary.Body[entry.firstIndex+1:]...)
		typecheck.Stmts(fn.Body)
	})
}
