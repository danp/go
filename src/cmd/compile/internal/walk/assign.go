// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package walk

import (
	"go/constant"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/src"
)

// walkAssign walks an OAS (AssignExpr) or OASOP (AssignOpExpr) node.
func walkAssign(init *ir.Nodes, n ir.Node) ir.Node {
	init.Append(n.PtrInit().Take()...)

	var left, right ir.Node
	switch n.Op() {
	case ir.OAS:
		n := n.(*ir.AssignStmt)
		left, right = n.X, n.Y
	case ir.OASOP:
		n := n.(*ir.AssignOpStmt)
		left, right = n.X, n.Y
	}

	// Recognize m[k] = append(m[k], ...) so we can reuse
	// the mapassign call.
	var mapAppend *ir.CallExpr
	if left.Op() == ir.OINDEXMAP && right.Op() == ir.OAPPEND {
		left := left.(*ir.IndexExpr)
		mapAppend = right.(*ir.CallExpr)
		if !ir.SameSafeExpr(left, mapAppend.Args[0]) {
			base.Fatalf("not same expressions: %v != %v", left, mapAppend.Args[0])
		}
	}

	left = walkExpr(left, init)
	left = safeExpr(left, init)
	if mapAppend != nil {
		mapAppend.Args[0] = left
	}

	if n.Op() == ir.OASOP {
		// Rewrite x op= y into x = x op y.
		n = ir.NewAssignStmt(base.Pos, left, typecheck.Expr(ir.NewBinaryExpr(base.Pos, n.(*ir.AssignOpStmt).AsOp, left, right)))
	} else {
		n.(*ir.AssignStmt).X = left
	}
	as := n.(*ir.AssignStmt)

	if oaslit(as, init) {
		return ir.NewBlockStmt(as.Pos(), nil)
	}

	if as.Y == nil {
		// TODO(austin): Check all "implicit zeroing"
		return as
	}

	if !base.Flag.Cfg.Instrumenting && ir.IsZero(as.Y) {
		return as
	}

	switch as.Y.Op() {
	default:
		as.Y = walkExpr(as.Y, init)

	case ir.ORECV:
		// x = <-c; as.Left is x, as.Right.Left is c.
		// order.stmt made sure x is addressable.
		recv := as.Y.(*ir.UnaryExpr)
		recv.X = walkExpr(recv.X, init)

		n1 := typecheck.NodAddr(as.X)
		r := recv.X // the channel
		return mkcall1(chanfn("chanrecv1", 2, r.Type()), nil, init, r, n1)

	case ir.OAPPEND:
		// x = append(...)
		call := as.Y.(*ir.CallExpr)
		if call.Type().Elem().NotInHeap() {
			base.Errorf("%v can't be allocated in Go; it is incomplete (or unallocatable)", call.Type().Elem())
		}
		var r ir.Node
		switch {
		case isAppendOfMake(call):
			// x = append(y, make([]T, y)...)
			r = extendSlice(call, init)
		case call.IsDDD:
			r = appendSlice(call, init) // also works for append(slice, string).
		default:
			r = walkAppend(call, init, as)
		}
		as.Y = r
		if r.Op() == ir.OAPPEND {
			// Left in place for back end.
			// Do not add a new write barrier.
			// Set up address of type for back end.
			r.(*ir.CallExpr).X = reflectdata.TypePtr(r.Type().Elem())
			return as
		}
		// Otherwise, lowered for race detector.
		// Treat as ordinary assignment.
	}

	if as.X != nil && as.Y != nil {
		return convas(as, init)
	}
	return as
}

// walkAssignDotType walks an OAS2DOTTYPE node.
func walkAssignDotType(n *ir.AssignListStmt, init *ir.Nodes) ir.Node {
	walkExprListSafe(n.Lhs, init)
	n.Rhs[0] = walkExpr(n.Rhs[0], init)
	return n
}

// walkAssignFunc walks an OAS2FUNC node.
func walkAssignFunc(init *ir.Nodes, n *ir.AssignListStmt) ir.Node {
	init.Append(n.PtrInit().Take()...)

	r := n.Rhs[0]
	walkExprListSafe(n.Lhs, init)
	r = walkExpr(r, init)

	if ir.IsIntrinsicCall(r.(*ir.CallExpr)) {
		n.Rhs = []ir.Node{r}
		return n
	}
	init.Append(r)

	ll := ascompatet(n.Lhs, r.Type())
	return ir.NewBlockStmt(src.NoXPos, ll)
}

// walkAssignList walks an OAS2 node.
func walkAssignList(init *ir.Nodes, n *ir.AssignListStmt) ir.Node {
	init.Append(n.PtrInit().Take()...)
	walkExprListSafe(n.Lhs, init)
	walkExprListSafe(n.Rhs, init)
	return ir.NewBlockStmt(src.NoXPos, ascompatee(ir.OAS, n.Lhs, n.Rhs, init))
}

// walkAssignMapRead walks an OAS2MAPR node.
func walkAssignMapRead(init *ir.Nodes, n *ir.AssignListStmt) ir.Node {
	init.Append(n.PtrInit().Take()...)

	r := n.Rhs[0].(*ir.IndexExpr)
	walkExprListSafe(n.Lhs, init)
	r.X = walkExpr(r.X, init)
	r.Index = walkExpr(r.Index, init)
	t := r.X.Type()

	fast := mapfast(t)
	var key ir.Node
	if fast != mapslow {
		// fast versions take key by value
		key = r.Index
	} else {
		// standard version takes key by reference
		// order.expr made sure key is addressable.
		key = typecheck.NodAddr(r.Index)
	}

	// from:
	//   a,b = m[i]
	// to:
	//   var,b = mapaccess2*(t, m, i)
	//   a = *var
	a := n.Lhs[0]

	var call *ir.CallExpr
	if w := t.Elem().Width; w <= zeroValSize {
		fn := mapfn(mapaccess2[fast], t)
		call = mkcall1(fn, fn.Type().Results(), init, reflectdata.TypePtr(t), r.X, key)
	} else {
		fn := mapfn("mapaccess2_fat", t)
		z := reflectdata.ZeroAddr(w)
		call = mkcall1(fn, fn.Type().Results(), init, reflectdata.TypePtr(t), r.X, key, z)
	}

	// mapaccess2* returns a typed bool, but due to spec changes,
	// the boolean result of i.(T) is now untyped so we make it the
	// same type as the variable on the lhs.
	if ok := n.Lhs[1]; !ir.IsBlank(ok) && ok.Type().IsBoolean() {
		call.Type().Field(1).Type = ok.Type()
	}
	n.Rhs = []ir.Node{call}
	n.SetOp(ir.OAS2FUNC)

	// don't generate a = *var if a is _
	if ir.IsBlank(a) {
		return walkExpr(typecheck.Stmt(n), init)
	}

	var_ := typecheck.Temp(types.NewPtr(t.Elem()))
	var_.SetTypecheck(1)
	var_.MarkNonNil() // mapaccess always returns a non-nil pointer

	n.Lhs[0] = var_
	init.Append(walkExpr(n, init))

	as := ir.NewAssignStmt(base.Pos, a, ir.NewStarExpr(base.Pos, var_))
	return walkExpr(typecheck.Stmt(as), init)
}

// walkAssignRecv walks an OAS2RECV node.
func walkAssignRecv(init *ir.Nodes, n *ir.AssignListStmt) ir.Node {
	init.Append(n.PtrInit().Take()...)

	r := n.Rhs[0].(*ir.UnaryExpr) // recv
	walkExprListSafe(n.Lhs, init)
	r.X = walkExpr(r.X, init)
	var n1 ir.Node
	if ir.IsBlank(n.Lhs[0]) {
		n1 = typecheck.NodNil()
	} else {
		n1 = typecheck.NodAddr(n.Lhs[0])
	}
	fn := chanfn("chanrecv2", 2, r.X.Type())
	ok := n.Lhs[1]
	call := mkcall1(fn, types.Types[types.TBOOL], init, r.X, n1)
	return typecheck.Stmt(ir.NewAssignStmt(base.Pos, ok, call))
}

// walkReturn walks an ORETURN node.
func walkReturn(n *ir.ReturnStmt) ir.Node {
	ir.CurFunc.NumReturns++
	if len(n.Results) == 0 {
		return n
	}
	if (ir.HasNamedResults(ir.CurFunc) && len(n.Results) > 1) || paramoutheap(ir.CurFunc) {
		// assign to the function out parameters,
		// so that ascompatee can fix up conflicts
		var rl []ir.Node

		for _, ln := range ir.CurFunc.Dcl {
			cl := ln.Class_
			if cl == ir.PAUTO || cl == ir.PAUTOHEAP {
				break
			}
			if cl == ir.PPARAMOUT {
				var ln ir.Node = ln
				if ir.IsParamStackCopy(ln) {
					ln = walkExpr(typecheck.Expr(ir.NewStarExpr(base.Pos, ln.Name().Heapaddr)), nil)
				}
				rl = append(rl, ln)
			}
		}

		if got, want := len(n.Results), len(rl); got != want {
			// order should have rewritten multi-value function calls
			// with explicit OAS2FUNC nodes.
			base.Fatalf("expected %v return arguments, have %v", want, got)
		}

		// move function calls out, to make ascompatee's job easier.
		walkExprListSafe(n.Results, n.PtrInit())

		n.Results.Set(ascompatee(n.Op(), rl, n.Results, n.PtrInit()))
		return n
	}
	walkExprList(n.Results, n.PtrInit())

	// For each return parameter (lhs), assign the corresponding result (rhs).
	lhs := ir.CurFunc.Type().Results()
	rhs := n.Results
	res := make([]ir.Node, lhs.NumFields())
	for i, nl := range lhs.FieldSlice() {
		nname := ir.AsNode(nl.Nname)
		if ir.IsParamHeapCopy(nname) {
			nname = nname.Name().Stackcopy
		}
		a := ir.NewAssignStmt(base.Pos, nname, rhs[i])
		res[i] = convas(a, n.PtrInit())
	}
	n.Results.Set(res)
	return n
}

// fncall reports whether assigning an rvalue of type rt to an lvalue l might involve a function call.
func fncall(l ir.Node, rt *types.Type) bool {
	if l.HasCall() || l.Op() == ir.OINDEXMAP {
		return true
	}
	if types.Identical(l.Type(), rt) {
		return false
	}
	// There might be a conversion required, which might involve a runtime call.
	return true
}

func ascompatee(op ir.Op, nl, nr []ir.Node, init *ir.Nodes) []ir.Node {
	// check assign expression list to
	// an expression list. called in
	//	expr-list = expr-list

	// ensure order of evaluation for function calls
	for i := range nl {
		nl[i] = safeExpr(nl[i], init)
	}
	for i1 := range nr {
		nr[i1] = safeExpr(nr[i1], init)
	}

	var nn []*ir.AssignStmt
	i := 0
	for ; i < len(nl); i++ {
		if i >= len(nr) {
			break
		}
		// Do not generate 'x = x' during return. See issue 4014.
		if op == ir.ORETURN && ir.SameSafeExpr(nl[i], nr[i]) {
			continue
		}
		nn = append(nn, ascompatee1(nl[i], nr[i], init))
	}

	// cannot happen: caller checked that lists had same length
	if i < len(nl) || i < len(nr) {
		var nln, nrn ir.Nodes
		nln.Set(nl)
		nrn.Set(nr)
		base.Fatalf("error in shape across %+v %v %+v / %d %d [%s]", nln, op, nrn, len(nl), len(nr), ir.FuncName(ir.CurFunc))
	}
	return reorder3(nn)
}

func ascompatee1(l ir.Node, r ir.Node, init *ir.Nodes) *ir.AssignStmt {
	// convas will turn map assigns into function calls,
	// making it impossible for reorder3 to work.
	n := ir.NewAssignStmt(base.Pos, l, r)

	if l.Op() == ir.OINDEXMAP {
		return n
	}

	return convas(n, init)
}

// check assign type list to
// an expression list. called in
//	expr-list = func()
func ascompatet(nl ir.Nodes, nr *types.Type) []ir.Node {
	if len(nl) != nr.NumFields() {
		base.Fatalf("ascompatet: assignment count mismatch: %d = %d", len(nl), nr.NumFields())
	}

	var nn, mm ir.Nodes
	for i, l := range nl {
		if ir.IsBlank(l) {
			continue
		}
		r := nr.Field(i)

		// Any assignment to an lvalue that might cause a function call must be
		// deferred until all the returned values have been read.
		if fncall(l, r.Type) {
			tmp := ir.Node(typecheck.Temp(r.Type))
			tmp = typecheck.Expr(tmp)
			a := convas(ir.NewAssignStmt(base.Pos, l, tmp), &mm)
			mm.Append(a)
			l = tmp
		}

		res := ir.NewResultExpr(base.Pos, nil, types.BADWIDTH)
		res.Offset = base.Ctxt.FixedFrameSize() + r.Offset
		res.SetType(r.Type)
		res.SetTypecheck(1)

		a := convas(ir.NewAssignStmt(base.Pos, l, res), &nn)
		updateHasCall(a)
		if a.HasCall() {
			ir.Dump("ascompatet ucount", a)
			base.Fatalf("ascompatet: too many function calls evaluating parameters")
		}

		nn.Append(a)
	}
	return append(nn, mm...)
}

// reorder3
// from ascompatee
//	a,b = c,d
// simultaneous assignment. there cannot
// be later use of an earlier lvalue.
//
// function calls have been removed.
func reorder3(all []*ir.AssignStmt) []ir.Node {
	// If a needed expression may be affected by an
	// earlier assignment, make an early copy of that
	// expression and use the copy instead.
	var early []ir.Node

	var mapinit ir.Nodes
	for i, n := range all {
		l := n.X

		// Save subexpressions needed on left side.
		// Drill through non-dereferences.
		for {
			switch ll := l; ll.Op() {
			case ir.ODOT:
				ll := ll.(*ir.SelectorExpr)
				l = ll.X
				continue
			case ir.OPAREN:
				ll := ll.(*ir.ParenExpr)
				l = ll.X
				continue
			case ir.OINDEX:
				ll := ll.(*ir.IndexExpr)
				if ll.X.Type().IsArray() {
					ll.Index = reorder3save(ll.Index, all, i, &early)
					l = ll.X
					continue
				}
			}
			break
		}

		switch l.Op() {
		default:
			base.Fatalf("reorder3 unexpected lvalue %v", l.Op())

		case ir.ONAME:
			break

		case ir.OINDEX, ir.OINDEXMAP:
			l := l.(*ir.IndexExpr)
			l.X = reorder3save(l.X, all, i, &early)
			l.Index = reorder3save(l.Index, all, i, &early)
			if l.Op() == ir.OINDEXMAP {
				all[i] = convas(all[i], &mapinit)
			}

		case ir.ODEREF:
			l := l.(*ir.StarExpr)
			l.X = reorder3save(l.X, all, i, &early)
		case ir.ODOTPTR:
			l := l.(*ir.SelectorExpr)
			l.X = reorder3save(l.X, all, i, &early)
		}

		// Save expression on right side.
		all[i].Y = reorder3save(all[i].Y, all, i, &early)
	}

	early = append(mapinit, early...)
	for _, as := range all {
		early = append(early, as)
	}
	return early
}

// if the evaluation of *np would be affected by the
// assignments in all up to but not including the ith assignment,
// copy into a temporary during *early and
// replace *np with that temp.
// The result of reorder3save MUST be assigned back to n, e.g.
// 	n.Left = reorder3save(n.Left, all, i, early)
func reorder3save(n ir.Node, all []*ir.AssignStmt, i int, early *[]ir.Node) ir.Node {
	if !aliased(n, all[:i]) {
		return n
	}

	q := ir.Node(typecheck.Temp(n.Type()))
	as := typecheck.Stmt(ir.NewAssignStmt(base.Pos, q, n))
	*early = append(*early, as)
	return q
}

// Is it possible that the computation of r might be
// affected by assignments in all?
func aliased(r ir.Node, all []*ir.AssignStmt) bool {
	if r == nil {
		return false
	}

	// Treat all fields of a struct as referring to the whole struct.
	// We could do better but we would have to keep track of the fields.
	for r.Op() == ir.ODOT {
		r = r.(*ir.SelectorExpr).X
	}

	// Look for obvious aliasing: a variable being assigned
	// during the all list and appearing in n.
	// Also record whether there are any writes to addressable
	// memory (either main memory or variables whose addresses
	// have been taken).
	memwrite := false
	for _, as := range all {
		// We can ignore assignments to blank.
		if ir.IsBlank(as.X) {
			continue
		}

		lv := ir.OuterValue(as.X)
		if lv.Op() != ir.ONAME {
			memwrite = true
			continue
		}
		l := lv.(*ir.Name)

		switch l.Class_ {
		default:
			base.Fatalf("unexpected class: %v, %v", l, l.Class_)

		case ir.PAUTOHEAP, ir.PEXTERN:
			memwrite = true
			continue

		case ir.PAUTO, ir.PPARAM, ir.PPARAMOUT:
			if l.Name().Addrtaken() {
				memwrite = true
				continue
			}

			if refersToName(l, r) {
				// Direct hit: l appears in r.
				return true
			}
		}
	}

	// The variables being written do not appear in r.
	// However, r might refer to computed addresses
	// that are being written.

	// If no computed addresses are affected by the writes, no aliasing.
	if !memwrite {
		return false
	}

	// If r does not refer to any variables whose addresses have been taken,
	// then the only possible writes to r would be directly to the variables,
	// and we checked those above, so no aliasing problems.
	if !anyAddrTaken(r) {
		return false
	}

	// Otherwise, both the writes and r refer to computed memory addresses.
	// Assume that they might conflict.
	return true
}

// anyAddrTaken reports whether the evaluation n,
// which appears on the left side of an assignment,
// may refer to variables whose addresses have been taken.
func anyAddrTaken(n ir.Node) bool {
	return ir.Any(n, func(n ir.Node) bool {
		switch n.Op() {
		case ir.ONAME:
			n := n.(*ir.Name)
			return n.Class_ == ir.PEXTERN || n.Class_ == ir.PAUTOHEAP || n.Name().Addrtaken()

		case ir.ODOT: // but not ODOTPTR - should have been handled in aliased.
			base.Fatalf("anyAddrTaken unexpected ODOT")

		case ir.OADD,
			ir.OAND,
			ir.OANDAND,
			ir.OANDNOT,
			ir.OBITNOT,
			ir.OCONV,
			ir.OCONVIFACE,
			ir.OCONVNOP,
			ir.ODIV,
			ir.ODOTTYPE,
			ir.OLITERAL,
			ir.OLSH,
			ir.OMOD,
			ir.OMUL,
			ir.ONEG,
			ir.ONIL,
			ir.OOR,
			ir.OOROR,
			ir.OPAREN,
			ir.OPLUS,
			ir.ORSH,
			ir.OSUB,
			ir.OXOR:
			return false
		}
		// Be conservative.
		return true
	})
}

// refersToName reports whether r refers to name.
func refersToName(name *ir.Name, r ir.Node) bool {
	return ir.Any(r, func(r ir.Node) bool {
		return r.Op() == ir.ONAME && r == name
	})
}

// refersToCommonName reports whether any name
// appears in common between l and r.
// This is called from sinit.go.
func refersToCommonName(l ir.Node, r ir.Node) bool {
	if l == nil || r == nil {
		return false
	}

	// This could be written elegantly as a Find nested inside a Find:
	//
	//	found := ir.Find(l, func(l ir.Node) interface{} {
	//		if l.Op() == ir.ONAME {
	//			return ir.Find(r, func(r ir.Node) interface{} {
	//				if r.Op() == ir.ONAME && l.Name() == r.Name() {
	//					return r
	//				}
	//				return nil
	//			})
	//		}
	//		return nil
	//	})
	//	return found != nil
	//
	// But that would allocate a new closure for the inner Find
	// for each name found on the left side.
	// It may not matter at all, but the below way of writing it
	// only allocates two closures, not O(|L|) closures.

	var doL, doR func(ir.Node) error
	var targetL *ir.Name
	doR = func(r ir.Node) error {
		if r.Op() == ir.ONAME && r.Name() == targetL {
			return stop
		}
		return ir.DoChildren(r, doR)
	}
	doL = func(l ir.Node) error {
		if l.Op() == ir.ONAME {
			l := l.(*ir.Name)
			targetL = l.Name()
			if doR(r) == stop {
				return stop
			}
		}
		return ir.DoChildren(l, doL)
	}
	return doL(l) == stop
}

// expand append(l1, l2...) to
//   init {
//     s := l1
//     n := len(s) + len(l2)
//     // Compare as uint so growslice can panic on overflow.
//     if uint(n) > uint(cap(s)) {
//       s = growslice(s, n)
//     }
//     s = s[:n]
//     memmove(&s[len(l1)], &l2[0], len(l2)*sizeof(T))
//   }
//   s
//
// l2 is allowed to be a string.
func appendSlice(n *ir.CallExpr, init *ir.Nodes) ir.Node {
	walkAppendArgs(n, init)

	l1 := n.Args[0]
	l2 := n.Args[1]
	l2 = cheapExpr(l2, init)
	n.Args[1] = l2

	var nodes ir.Nodes

	// var s []T
	s := typecheck.Temp(l1.Type())
	nodes.Append(ir.NewAssignStmt(base.Pos, s, l1)) // s = l1

	elemtype := s.Type().Elem()

	// n := len(s) + len(l2)
	nn := typecheck.Temp(types.Types[types.TINT])
	nodes.Append(ir.NewAssignStmt(base.Pos, nn, ir.NewBinaryExpr(base.Pos, ir.OADD, ir.NewUnaryExpr(base.Pos, ir.OLEN, s), ir.NewUnaryExpr(base.Pos, ir.OLEN, l2))))

	// if uint(n) > uint(cap(s))
	nif := ir.NewIfStmt(base.Pos, nil, nil, nil)
	nuint := typecheck.Conv(nn, types.Types[types.TUINT])
	scapuint := typecheck.Conv(ir.NewUnaryExpr(base.Pos, ir.OCAP, s), types.Types[types.TUINT])
	nif.Cond = ir.NewBinaryExpr(base.Pos, ir.OGT, nuint, scapuint)

	// instantiate growslice(typ *type, []any, int) []any
	fn := typecheck.LookupRuntime("growslice")
	fn = typecheck.SubstArgTypes(fn, elemtype, elemtype)

	// s = growslice(T, s, n)
	nif.Body = []ir.Node{ir.NewAssignStmt(base.Pos, s, mkcall1(fn, s.Type(), nif.PtrInit(), reflectdata.TypePtr(elemtype), s, nn))}
	nodes.Append(nif)

	// s = s[:n]
	nt := ir.NewSliceExpr(base.Pos, ir.OSLICE, s, nil, nn, nil)
	nt.SetBounded(true)
	nodes.Append(ir.NewAssignStmt(base.Pos, s, nt))

	var ncopy ir.Node
	if elemtype.HasPointers() {
		// copy(s[len(l1):], l2)
		slice := ir.NewSliceExpr(base.Pos, ir.OSLICE, s, ir.NewUnaryExpr(base.Pos, ir.OLEN, l1), nil, nil)
		slice.SetType(s.Type())

		ir.CurFunc.SetWBPos(n.Pos())

		// instantiate typedslicecopy(typ *type, dstPtr *any, dstLen int, srcPtr *any, srcLen int) int
		fn := typecheck.LookupRuntime("typedslicecopy")
		fn = typecheck.SubstArgTypes(fn, l1.Type().Elem(), l2.Type().Elem())
		ptr1, len1 := backingArrayPtrLen(cheapExpr(slice, &nodes))
		ptr2, len2 := backingArrayPtrLen(l2)
		ncopy = mkcall1(fn, types.Types[types.TINT], &nodes, reflectdata.TypePtr(elemtype), ptr1, len1, ptr2, len2)
	} else if base.Flag.Cfg.Instrumenting && !base.Flag.CompilingRuntime {
		// rely on runtime to instrument:
		//  copy(s[len(l1):], l2)
		// l2 can be a slice or string.
		slice := ir.NewSliceExpr(base.Pos, ir.OSLICE, s, ir.NewUnaryExpr(base.Pos, ir.OLEN, l1), nil, nil)
		slice.SetType(s.Type())

		ptr1, len1 := backingArrayPtrLen(cheapExpr(slice, &nodes))
		ptr2, len2 := backingArrayPtrLen(l2)

		fn := typecheck.LookupRuntime("slicecopy")
		fn = typecheck.SubstArgTypes(fn, ptr1.Type().Elem(), ptr2.Type().Elem())
		ncopy = mkcall1(fn, types.Types[types.TINT], &nodes, ptr1, len1, ptr2, len2, ir.NewInt(elemtype.Width))
	} else {
		// memmove(&s[len(l1)], &l2[0], len(l2)*sizeof(T))
		ix := ir.NewIndexExpr(base.Pos, s, ir.NewUnaryExpr(base.Pos, ir.OLEN, l1))
		ix.SetBounded(true)
		addr := typecheck.NodAddr(ix)

		sptr := ir.NewUnaryExpr(base.Pos, ir.OSPTR, l2)

		nwid := cheapExpr(typecheck.Conv(ir.NewUnaryExpr(base.Pos, ir.OLEN, l2), types.Types[types.TUINTPTR]), &nodes)
		nwid = ir.NewBinaryExpr(base.Pos, ir.OMUL, nwid, ir.NewInt(elemtype.Width))

		// instantiate func memmove(to *any, frm *any, length uintptr)
		fn := typecheck.LookupRuntime("memmove")
		fn = typecheck.SubstArgTypes(fn, elemtype, elemtype)
		ncopy = mkcall1(fn, nil, &nodes, addr, sptr, nwid)
	}
	ln := append(nodes, ncopy)

	typecheck.Stmts(ln)
	walkStmtList(ln)
	init.Append(ln...)
	return s
}

// isAppendOfMake reports whether n is of the form append(x , make([]T, y)...).
// isAppendOfMake assumes n has already been typechecked.
func isAppendOfMake(n ir.Node) bool {
	if base.Flag.N != 0 || base.Flag.Cfg.Instrumenting {
		return false
	}

	if n.Typecheck() == 0 {
		base.Fatalf("missing typecheck: %+v", n)
	}

	if n.Op() != ir.OAPPEND {
		return false
	}
	call := n.(*ir.CallExpr)
	if !call.IsDDD || len(call.Args) != 2 || call.Args[1].Op() != ir.OMAKESLICE {
		return false
	}

	mk := call.Args[1].(*ir.MakeExpr)
	if mk.Cap != nil {
		return false
	}

	// y must be either an integer constant or the largest possible positive value
	// of variable y needs to fit into an uint.

	// typecheck made sure that constant arguments to make are not negative and fit into an int.

	// The care of overflow of the len argument to make will be handled by an explicit check of int(len) < 0 during runtime.
	y := mk.Len
	if !ir.IsConst(y, constant.Int) && y.Type().Size() > types.Types[types.TUINT].Size() {
		return false
	}

	return true
}

// extendSlice rewrites append(l1, make([]T, l2)...) to
//   init {
//     if l2 >= 0 { // Empty if block here for more meaningful node.SetLikely(true)
//     } else {
//       panicmakeslicelen()
//     }
//     s := l1
//     n := len(s) + l2
//     // Compare n and s as uint so growslice can panic on overflow of len(s) + l2.
//     // cap is a positive int and n can become negative when len(s) + l2
//     // overflows int. Interpreting n when negative as uint makes it larger
//     // than cap(s). growslice will check the int n arg and panic if n is
//     // negative. This prevents the overflow from being undetected.
//     if uint(n) > uint(cap(s)) {
//       s = growslice(T, s, n)
//     }
//     s = s[:n]
//     lptr := &l1[0]
//     sptr := &s[0]
//     if lptr == sptr || !T.HasPointers() {
//       // growslice did not clear the whole underlying array (or did not get called)
//       hp := &s[len(l1)]
//       hn := l2 * sizeof(T)
//       memclr(hp, hn)
//     }
//   }
//   s
func extendSlice(n *ir.CallExpr, init *ir.Nodes) ir.Node {
	// isAppendOfMake made sure all possible positive values of l2 fit into an uint.
	// The case of l2 overflow when converting from e.g. uint to int is handled by an explicit
	// check of l2 < 0 at runtime which is generated below.
	l2 := typecheck.Conv(n.Args[1].(*ir.MakeExpr).Len, types.Types[types.TINT])
	l2 = typecheck.Expr(l2)
	n.Args[1] = l2 // walkAppendArgs expects l2 in n.List.Second().

	walkAppendArgs(n, init)

	l1 := n.Args[0]
	l2 = n.Args[1] // re-read l2, as it may have been updated by walkAppendArgs

	var nodes []ir.Node

	// if l2 >= 0 (likely happens), do nothing
	nifneg := ir.NewIfStmt(base.Pos, ir.NewBinaryExpr(base.Pos, ir.OGE, l2, ir.NewInt(0)), nil, nil)
	nifneg.Likely = true

	// else panicmakeslicelen()
	nifneg.Else = []ir.Node{mkcall("panicmakeslicelen", nil, init)}
	nodes = append(nodes, nifneg)

	// s := l1
	s := typecheck.Temp(l1.Type())
	nodes = append(nodes, ir.NewAssignStmt(base.Pos, s, l1))

	elemtype := s.Type().Elem()

	// n := len(s) + l2
	nn := typecheck.Temp(types.Types[types.TINT])
	nodes = append(nodes, ir.NewAssignStmt(base.Pos, nn, ir.NewBinaryExpr(base.Pos, ir.OADD, ir.NewUnaryExpr(base.Pos, ir.OLEN, s), l2)))

	// if uint(n) > uint(cap(s))
	nuint := typecheck.Conv(nn, types.Types[types.TUINT])
	capuint := typecheck.Conv(ir.NewUnaryExpr(base.Pos, ir.OCAP, s), types.Types[types.TUINT])
	nif := ir.NewIfStmt(base.Pos, ir.NewBinaryExpr(base.Pos, ir.OGT, nuint, capuint), nil, nil)

	// instantiate growslice(typ *type, old []any, newcap int) []any
	fn := typecheck.LookupRuntime("growslice")
	fn = typecheck.SubstArgTypes(fn, elemtype, elemtype)

	// s = growslice(T, s, n)
	nif.Body = []ir.Node{ir.NewAssignStmt(base.Pos, s, mkcall1(fn, s.Type(), nif.PtrInit(), reflectdata.TypePtr(elemtype), s, nn))}
	nodes = append(nodes, nif)

	// s = s[:n]
	nt := ir.NewSliceExpr(base.Pos, ir.OSLICE, s, nil, nn, nil)
	nt.SetBounded(true)
	nodes = append(nodes, ir.NewAssignStmt(base.Pos, s, nt))

	// lptr := &l1[0]
	l1ptr := typecheck.Temp(l1.Type().Elem().PtrTo())
	tmp := ir.NewUnaryExpr(base.Pos, ir.OSPTR, l1)
	nodes = append(nodes, ir.NewAssignStmt(base.Pos, l1ptr, tmp))

	// sptr := &s[0]
	sptr := typecheck.Temp(elemtype.PtrTo())
	tmp = ir.NewUnaryExpr(base.Pos, ir.OSPTR, s)
	nodes = append(nodes, ir.NewAssignStmt(base.Pos, sptr, tmp))

	// hp := &s[len(l1)]
	ix := ir.NewIndexExpr(base.Pos, s, ir.NewUnaryExpr(base.Pos, ir.OLEN, l1))
	ix.SetBounded(true)
	hp := typecheck.ConvNop(typecheck.NodAddr(ix), types.Types[types.TUNSAFEPTR])

	// hn := l2 * sizeof(elem(s))
	hn := typecheck.Conv(ir.NewBinaryExpr(base.Pos, ir.OMUL, l2, ir.NewInt(elemtype.Width)), types.Types[types.TUINTPTR])

	clrname := "memclrNoHeapPointers"
	hasPointers := elemtype.HasPointers()
	if hasPointers {
		clrname = "memclrHasPointers"
		ir.CurFunc.SetWBPos(n.Pos())
	}

	var clr ir.Nodes
	clrfn := mkcall(clrname, nil, &clr, hp, hn)
	clr.Append(clrfn)

	if hasPointers {
		// if l1ptr == sptr
		nifclr := ir.NewIfStmt(base.Pos, ir.NewBinaryExpr(base.Pos, ir.OEQ, l1ptr, sptr), nil, nil)
		nifclr.Body = clr
		nodes = append(nodes, nifclr)
	} else {
		nodes = append(nodes, clr...)
	}

	typecheck.Stmts(nodes)
	walkStmtList(nodes)
	init.Append(nodes...)
	return s
}
