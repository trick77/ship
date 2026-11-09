package conform

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"
)

// flow finds the field selectors a row's Scan fills, across one module's packages: &x.F among its dest arguments,
// also inside a wrapper call there (timeAt(&p.At), boxed(…), a conversion), and the elements of a dest slice that
// reaches it. A slice is followed through locals, fields and appends (a builder's tuple result included), a dest
// builder's returns (sourceDest(&j.Source): its own literal credits, not the field's holder; one called on an
// interface: every method of that name; one passed as a func value or closure), a helper's parameters, a closure's
// included (scanRun(rows, &x.F) and scanRun(rows, extra...) forwarding to Scan(d...)); a pointer element through its
// assignments (not a call's result: that points elsewhere) and a builder's parameters (stamps(&p.At)). A row's Scan
// handed on as a func value (scanBrief(row.Scan, …), scan := rows.Scan, hop by hop) is a Scan where that variable is
// called; a slice pgx scans itself (ForEachRow's scans) is a dest slice. A []any reaching no Scan fills nothing: a
// query's arguments are no row. Not context-sensitive: a builder or helper one Scan reaches counts at every call, a
// query's included. Unfollowed (no row seen): a slice in a map or an index, a builder a func-typed field holds.
type flow struct {
	own     map[*types.Package]bool
	funcs   map[*types.Func]funcBody
	calls   map[any][]site // by callee: a *types.Func, or the *types.Var holding a func value
	assigns map[*types.Var][]site
	params  map[*types.Var]param
	scans   []scan
	slices  []site // dest slices a library scans
	seen    map[any]bool
	out     []dest
}

type (
	// funcBody is a function's body and the package info that types it.
	funcBody struct {
		body *ast.BlockStmt
		info *types.Info
	}
	// site is an expression (a call, an assignment's right side) and its package's info.
	site struct {
		e    ast.Expr
		info *types.Info
	}
	// param is the i-th parameter of the function calls keys by fn; variadic: the last, ...T.
	param struct {
		fn       any
		i        int
		variadic bool
	}
	// scan is a row Scan call, i its dest parameter's index among the call's arguments.
	scan struct {
		site
		i int
	}
	// dest is a field selector a scan fills.
	dest struct {
		sel  *ast.SelectorExpr
		info *types.Info
	}
	// sliceVar, ptrVar, funcVar and scanFuncVar key seen: a []any variable (or field) followed as a dest slice, a
	// pointer one as a dest, a func one as a builder, a func one asked whether it holds a row's Scan.
	sliceVar    struct{ v *types.Var }
	ptrVar      struct{ v *types.Var }
	funcVar     struct{ v *types.Var }
	scanFuncVar struct{ v *types.Var }
)

// pgxPath prefixes pgx's packages: their scans slices (ForEachRow, QueryFunc) are filled like a Scan's dest.
const pgxPath = "github.com/jackc/pgx"

// newFlow indexes the packages' functions, calls, assignments and row Scans; own holds the module's packages.
func newFlow(pkgs []*packages.Package, own map[*types.Package]bool) *flow {
	fl := &flow{own: own, funcs: map[*types.Func]funcBody{}, calls: map[any][]site{},
		assigns: map[*types.Var][]site{}, params: map[*types.Var]param{}, seen: map[any]bool{}}
	for _, p := range pkgs {
		info := p.TypesInfo
		for _, af := range p.Syntax {
			ast.Inspect(af, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncDecl:
					if fn, ok := info.Defs[x.Name].(*types.Func); ok {
						fl.funcs[fn] = funcBody{x.Body, info}
						fl.addParams(fn, fn.Signature())
					}
				case *ast.CallExpr:
					fl.call(x, info)
				case *ast.AssignStmt:
					for i, l := range x.Lhs {
						switch l := ast.Unparen(l).(type) {
						case *ast.Ident:
							fl.assign(info.ObjectOf(l), len(x.Lhs), x.Rhs, i, info)
						case *ast.SelectorExpr: // a field: c.dest = []any{…}
							fl.assign(info.Uses[l.Sel], len(x.Lhs), x.Rhs, i, info)
						}
					}
				case *ast.ValueSpec:
					for i, id := range x.Names {
						fl.assign(info.Defs[id], len(x.Names), x.Values, i, info)
					}
				}
				return true
			})
		}
	}
	for key, ss := range fl.calls {
		if v, ok := key.(*types.Var); ok {
			if i, ok := destParam(v.Type()); ok && fl.scanFunc(v) {
				for _, s := range ss {
					fl.scans = append(fl.scans, scan{s, i})
				}
			}
		}
	}
	return fl
}

// addParams indexes sig's parameters as fn's, fn a *types.Func or the *types.Var a closure is assigned to.
func (fl *flow) addParams(fn any, sig *types.Signature) {
	ps := sig.Params()
	for i := range ps.Len() {
		fl.params[ps.At(i)] = param{fn, i, sig.Variadic() && i == ps.Len()-1}
	}
}

// call indexes c: by callee, as a row Scan, as a pgx call taking a dest slice.
func (fl *flow) call(c *ast.CallExpr, info *types.Info) {
	fn := callee(info, c)
	if fn != nil {
		fl.calls[fn] = append(fl.calls[fn], site{c, info})
	} else if id, ok := ast.Unparen(c.Fun).(*ast.Ident); ok {
		if v, ok := info.Uses[id].(*types.Var); ok {
			fl.calls[v] = append(fl.calls[v], site{c, info})
		}
	}
	if i, ok := fl.rowScan(info, c); ok {
		fl.scans = append(fl.scans, scan{site{c, info}, i})
	}
	if fn != nil && fn.Pkg() != nil && strings.HasPrefix(fn.Pkg().Path(), pgxPath) {
		ps := fn.Signature().Params()
		for i := range min(ps.Len(), len(c.Args)) {
			if ps.At(i).Name() == "scans" && anySlice(ps.At(i).Type()) {
				fl.slices = append(fl.slices, site{c.Args[i], info})
			}
		}
	}
}

// assign indexes the i-th of n left sides, obj: rhs[i], or for a []any a tuple's one call (d, err := r.dests()). A
// closure assigned is indexed as its variable's function: its parameters are followed to the variable's calls.
func (fl *flow) assign(obj types.Object, n int, rhs []ast.Expr, i int, info *types.Info) {
	v, ok := obj.(*types.Var)
	if !ok {
		return
	}
	var e ast.Expr
	switch {
	case len(rhs) == n:
		e = rhs[i]
	case len(rhs) == 1 && anySlice(v.Type()):
		e = rhs[0]
	default:
		return
	}
	fl.assigns[v] = append(fl.assigns[v], site{e, info})
	if lit, ok := ast.Unparen(e).(*ast.FuncLit); ok {
		if sig, ok := info.TypeOf(lit).(*types.Signature); ok {
			fl.addParams(v, sig)
		}
	}
}

// rowScan: c calls a row's Scan, a Scan(…, dest ...any): sql.Row(s), pgx.Row(s), the rows a DBTX interface or a pool
// field returns, an own scanner, a method expression, a package's Scan func of this module (fmt.Scan reads no row).
// The name picks the call, the type-checked signature decides: bufio.Scanner's Scan() or a sql.Scanner's Scan(any)
// fills no row. i is dest's index among c's arguments.
func (fl *flow) rowScan(info *types.Info, c *ast.CallExpr) (i int, ok bool) {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	if !ok || !fl.scanSel(info, sel) {
		return 0, false
	}
	return destParam(info.TypeOf(c.Fun))
}

// scanSel: sel names a Scan(…, dest ...any), a method or this module's func; a func value or a call alike.
func (fl *flow) scanSel(info *types.Info, sel *ast.SelectorExpr) bool {
	if sel.Sel.Name != "Scan" {
		return false
	}
	if fn, ok := info.Uses[sel.Sel].(*types.Func); ok && fn.Signature().Recv() == nil && !fl.own[fn.Pkg()] {
		return false
	}
	_, ok := destParam(info.TypeOf(sel))
	return ok
}

// scanFunc: v holds a row's Scan as a func value, assigned or, as a parameter, passed by a caller, directly or
// through another such variable (scan := row.Scan; scanBrief(scan, …)).
func (fl *flow) scanFunc(v *types.Var) bool {
	if fl.seen[scanFuncVar{v}] {
		return false
	}
	fl.seen[scanFuncVar{v}] = true
	from := func(s site) bool {
		switch x := ast.Unparen(s.e).(type) {
		case *ast.SelectorExpr:
			return fl.scanSel(s.info, x)
		case *ast.Ident:
			w, ok := s.info.Uses[x].(*types.Var)
			return ok && fl.scanFunc(w)
		}
		return false
	}
	for _, s := range fl.assigns[v] {
		if from(s) {
			return true
		}
	}
	for _, a := range fl.callerArgs(v) {
		if from(a.site) {
			return true
		}
	}
	return false
}

// destParam: t is a func(…, dest ...any); i is dest's index.
func destParam(t types.Type) (i int, ok bool) {
	if t == nil {
		return 0, false
	}
	sig, ok := t.Underlying().(*types.Signature)
	if !ok || !sig.Variadic() {
		return 0, false
	}
	i = sig.Params().Len() - 1
	return i, anySlice(sig.Params().At(i).Type())
}

// callee is the function or method c calls by name (an instantiation's too), its generic origin; nil for a func
// value or a builtin.
func callee(info *types.Info, c *ast.CallExpr) *types.Func {
	f := ast.Unparen(c.Fun)
	switch x := f.(type) {
	case *ast.IndexExpr:
		f = x.X
	case *ast.IndexListExpr:
		f = x.X
	}
	var id *ast.Ident
	switch x := ast.Unparen(f).(type) {
	case *ast.Ident:
		id = x
	case *ast.SelectorExpr:
		id = x.Sel
	}
	if id == nil {
		return nil
	}
	if fn, ok := info.Uses[id].(*types.Func); ok {
		return fn.Origin()
	}
	return nil
}

// arg is a caller's argument for a parameter; spread: a variadic one's whole slice (x...), else one element.
type arg struct {
	site
	spread bool
}

// callerArgs: for parameter v, each call's argument there, every one from there on for a variadic one. A method
// expression's call passes its receiver first.
func (fl *flow) callerArgs(v *types.Var) []arg {
	p, ok := fl.params[v]
	if !ok {
		return nil
	}
	var out []arg
	for _, s := range fl.calls[p.fn] {
		c := s.e.(*ast.CallExpr)
		i := p.i
		if sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr); ok {
			if m, ok := s.info.Selections[sel]; ok && m.Kind() == types.MethodExpr {
				i++
			}
		}
		switch {
		case i >= len(c.Args):
		case !p.variadic:
			out = append(out, arg{site{c.Args[i], s.info}, false})
		case c.Ellipsis.IsValid():
			out = append(out, arg{site{c.Args[i], s.info}, true})
		default:
			for _, a := range c.Args[i:] {
				out = append(out, arg{site{a, s.info}, false})
			}
		}
	}
	return out
}

// dests follows every row Scan's dest arguments and every dest slice a library scans.
func (fl *flow) dests() []dest {
	for _, s := range fl.scans {
		c := s.e.(*ast.CallExpr)
		if s.i >= len(c.Args) {
			continue
		}
		if c.Ellipsis.IsValid() {
			fl.slice(c.Args[s.i], s.info)
		} else {
			fl.elems(c.Args[s.i:], s.info)
		}
	}
	for _, s := range fl.slices {
		fl.slice(s.e, s.info)
	}
	return fl.out
}

// slice follows a dest slice to its elements.
func (fl *flow) slice(e ast.Expr, info *types.Info) {
	switch x := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		if anySlice(info.TypeOf(x)) {
			fl.elems(x.Elts, info)
		}
	case *ast.Ident:
		if v, ok := info.Uses[x].(*types.Var); ok {
			fl.sliceVar(v)
		}
	case *ast.SelectorExpr: // a field holding the slice (c.dest), or another package's variable
		if v, ok := info.Uses[x.Sel].(*types.Var); ok {
			fl.sliceVar(v)
		}
	case *ast.SliceExpr:
		fl.slice(x.X, info)
	case *ast.CallExpr:
		switch {
		case isAppend(info, x):
			fl.slice(x.Args[0], info)
			if x.Ellipsis.IsValid() {
				fl.slice(x.Args[1], info)
			} else {
				fl.elems(x.Args[1:], info)
			}
		case info.Types[x.Fun].IsType() && len(x.Args) == 1:
			fl.slice(x.Args[0], info)
		default:
			if fn := callee(info, x); fn != nil {
				fl.builder(fn)
			} else {
				fl.builderValue(x.Fun, info)
			}
		}
	}
}

// builderValue follows a builder called as a func value: a closure, a function named, a variable holding either
// (assigned, or a parameter its callers pass one to).
func (fl *flow) builderValue(e ast.Expr, info *types.Info) {
	switch x := ast.Unparen(e).(type) {
	case *ast.FuncLit:
		fl.returns(x.Body, info)
	case *ast.SelectorExpr:
		if fn, ok := info.Uses[x.Sel].(*types.Func); ok {
			fl.builder(fn.Origin())
		}
	case *ast.Ident:
		switch o := info.Uses[x].(type) {
		case *types.Func:
			fl.builder(o.Origin())
		case *types.Var:
			if fl.seen[funcVar{o}] {
				return
			}
			fl.seen[funcVar{o}] = true
			for _, s := range fl.assigns[o] {
				fl.builderValue(s.e, s.info)
			}
			for _, a := range fl.callerArgs(o) {
				fl.builderValue(a.e, a.info)
			}
		}
	}
}

// sliceVar follows a dest slice variable: its assignments, and as a parameter the callers' arguments (a variadic
// one's: each a dest, or the slice spread).
func (fl *flow) sliceVar(v *types.Var) {
	if fl.seen[sliceVar{v}] {
		return
	}
	fl.seen[sliceVar{v}] = true
	for _, s := range fl.assigns[v] {
		fl.slice(s.e, s.info)
	}
	variadic := fl.params[v].variadic
	for _, a := range fl.callerArgs(v) {
		if variadic && !a.spread {
			fl.elems([]ast.Expr{a.e}, a.info)
		} else {
			fl.slice(a.e, a.info)
		}
	}
}

// builder follows a dest builder, a function returning the slice: its []any results. An interface's or a type
// parameter's method has no body: every method of the module by that name returning a []any stands in for it.
func (fl *flow) builder(fn *types.Func) {
	if fl.seen[fn] {
		return
	}
	fl.seen[fn] = true
	if recv := fn.Signature().Recv(); recv != nil && types.IsInterface(recv.Type()) {
		for impl := range fl.funcs {
			if r := impl.Signature().Recv(); impl.Name() == fn.Name() && r != nil && !types.IsInterface(r.Type()) {
				fl.builder(impl)
			}
		}
		return
	}
	d, ok := fl.funcs[fn]
	if !ok || d.body == nil {
		return
	}
	rs := fn.Signature().Results()
	for i := range rs.Len() {
		if anySlice(rs.At(i).Type()) {
			fl.sliceVar(rs.At(i)) // a named result's assignments
		}
	}
	fl.returns(d.body, d.info)
}

// returns follows a builder body's returns: a []any result, or another builder's call whose tuple holds one
// (return r.base()).
func (fl *flow) returns(body *ast.BlockStmt, info *types.Info) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			return false // its returns are its own
		case *ast.ReturnStmt:
			for _, r := range x.Results {
				if t := info.TypeOf(r); anySlice(t) || len(x.Results) == 1 && tupleHolds(t) {
					fl.slice(r, info)
				}
			}
		}
		return true
	})
}

// tupleHolds: t is a tuple with a []any.
func tupleHolds(t types.Type) bool {
	tu, ok := t.(*types.Tuple)
	if !ok {
		return false
	}
	for i := range tu.Len() {
		if anySlice(tu.At(i).Type()) {
			return true
		}
	}
	return false
}

// elems takes dest elements: &x.F, a wrapper call's arguments (one not itself a []any), a pointer variable.
func (fl *flow) elems(args []ast.Expr, info *types.Info) {
	for _, a := range args {
		switch x := ast.Unparen(a).(type) {
		case *ast.UnaryExpr:
			if s, ok := ast.Unparen(x.X).(*ast.SelectorExpr); ok && x.Op == token.AND {
				fl.out = append(fl.out, dest{s, info})
			}
		case *ast.CallExpr:
			if !anySlice(info.TypeOf(x)) {
				fl.elems(x.Args, info)
			}
		case *ast.Ident:
			if v, ok := info.Uses[x].(*types.Var); ok {
				if _, ok := v.Type().Underlying().(*types.Pointer); ok {
					fl.ptrVar(v)
				}
			}
		}
	}
}

// ptrVar follows a dest pointer variable: its assignments, and as a parameter the callers' arguments.
func (fl *flow) ptrVar(v *types.Var) {
	if fl.seen[ptrVar{v}] {
		return
	}
	fl.seen[ptrVar{v}] = true
	for _, s := range fl.assigns[v] {
		// A pointer a call returns (p := cache.slot(&cfg.Key)) points at its own storage, not its arguments'; a
		// conversion's is its operand.
		if c, ok := ast.Unparen(s.e).(*ast.CallExpr); ok && !s.info.Types[c.Fun].IsType() {
			continue
		}
		fl.elems([]ast.Expr{s.e}, s.info)
	}
	for _, a := range fl.callerArgs(v) {
		fl.elems([]ast.Expr{a.e}, a.info)
	}
}
