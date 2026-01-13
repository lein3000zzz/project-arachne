package pageparser

import (
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"
)

type nodeVisitor interface {
	visit(ast.Node)
}

type jsCollector struct {
	varInits    map[string]ast.Expression
	assigns     []*ast.AssignExpression
	calls       []*ast.CallExpression
	objLiterals map[string]*ast.ObjectLiteral
	xhrVars     map[string]struct{}
}

func newJSCollector() *jsCollector {
	return &jsCollector{
		varInits:    make(map[string]ast.Expression),
		assigns:     make([]*ast.AssignExpression, 0),
		calls:       make([]*ast.CallExpression, 0),
		objLiterals: make(map[string]*ast.ObjectLiteral),
		xhrVars:     make(map[string]struct{}),
	}
}

func (c *jsCollector) visit(n ast.Node) {
	switch node := n.(type) {
	case *ast.VariableDeclaration:
		for _, raw := range node.List {
			name, init := varDeclNameInit(raw)

			if name == "" || init == nil {
				continue
			}

			c.varInits[name] = init

			switch typedInit := init.(type) {
			case *ast.ObjectLiteral:
				c.objLiterals[name] = typedInit
			case *ast.NewExpression:
				if identIs(typedInit.Callee, "XMLHttpRequest") {
					c.xhrVars[name] = struct{}{}
				}
			}
		}
	case *ast.AssignExpression:
		c.assigns = append(c.assigns, node)
		if identifier, ok := node.Left.(*ast.Identifier); ok {
			if newExpr, ok2 := node.Right.(*ast.NewExpression); ok2 && identIs(newExpr.Callee, "XMLHttpRequest") {
				c.xhrVars[identifier.Name.String()] = struct{}{}
			}
		}
	case *ast.CallExpression:
		c.calls = append(c.calls, node)
	}
}

type exprURLVisitor struct {
	p       *ParserBasic
	vars    map[string]string
	objects map[string]map[string]string
	adder   *linkAdder
}

func (v *exprURLVisitor) visit(n ast.Node) {
	if expr, ok := n.(ast.Expression); ok {
		if s, ok2 := v.p.evalStringExpr(expr, v.vars, v.objects); ok2 && v.p.looksLikeRelativePath(s) {
			v.adder.Add(s)
		}
	}
}

type linkAdder struct {
	p    *ParserBasic
	base *url.URL
	seen map[string]struct{}
}

func newLinkAdder(p *ParserBasic, base *url.URL, seen map[string]struct{}) *linkAdder {
	return &linkAdder{
		p:    p,
		base: base,
		seen: seen,
	}
}

func (a *linkAdder) Add(s string) {
	if !a.p.isURLCandidate(s) {
		return
	}

	if normalized := a.p.normalizeURL(s); normalized != "" {
		a.p.resolveAndAdd(normalized, a.seen, a.base)
	}
}

func (p *ParserBasic) ExtractLinksFromJS(baseURL, src string) ([]string, error) {
	var base *url.URL
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil {
			base = u
		} else if p.Logger != nil {
			p.Logger.Warnw("invalid base URL, skipping resolution", "base", baseURL, "err", err)
		}
	}

	parsedJS, err := parser.ParseFile(nil, "", src, 0)
	if err != nil {
		p.Logger.Errorw("js parse error", "err", err)
		return nil, err
	}

	collector := newJSCollector()
	p.walk(parsedJS, collector)

	vars := make(map[string]string)
	objects := make(map[string]map[string]string)

	for name, objectLiteral := range collector.objLiterals {
		objects[name] = p.collectObjectLiteralStrings(objectLiteral, vars, objects)
	}

	changed := true
	for changed {
		changed = false

		for name, expression := range collector.varInits {
			if _, ok := vars[name]; ok {
				if ol, ok2 := expression.(*ast.ObjectLiteral); ok2 {
					dst := objects[name]
					if dst == nil {
						dst = make(map[string]string)
						objects[name] = dst
					}

					srcMap := p.collectObjectLiteralStrings(ol, vars, objects)
					if mergeChanged(dst, srcMap) {
						changed = true
					}
				}
				continue
			}
			if s, ok := p.evalStringExpr(expression, vars, objects); ok {
				vars[name] = s
				changed = true

				continue
			}
			if ol, ok := expression.(*ast.ObjectLiteral); ok {
				dst := objects[name]
				if dst == nil {
					dst = make(map[string]string)
					objects[name] = dst
				}

				srcMap := p.collectObjectLiteralStrings(ol, vars, objects)
				if mergeChanged(dst, srcMap) {
					changed = true
				}
			}
		}

		for _, assignExpression := range collector.assigns {
			rightStr, rightOk := p.evalStringExpr(assignExpression.Right, vars, objects)

			switch left := assignExpression.Left.(type) {
			case *ast.Identifier:
				if rightOk {
					leftName := left.Name.String()
					if prev, ex := vars[leftName]; !ex || prev != rightStr {
						vars[leftName] = rightStr
						changed = true
					}
				}
			case *ast.DotExpression:
				if id, ok := left.Left.(*ast.Identifier); ok && rightOk {
					idName := id.Name.String()

					m := objects[idName]
					if m == nil {
						m = map[string]string{}
						objects[idName] = m
					}

					prop := left.Identifier.Name.String()
					if prev, ex := m[prop]; !ex || prev != rightStr {
						m[prop] = rightStr
						changed = true
					}
				}
			case *ast.BracketExpression:
				if id, ok := left.Left.(*ast.Identifier); ok {
					if key, ok2 := p.evalStringExpr(left.Member, vars, objects); ok2 && rightOk {
						idName := id.Name.String()
						m := objects[idName]
						if m == nil {
							m = map[string]string{}
							objects[idName] = m
						}
						if prev, ex := m[key]; !ex || prev != rightStr {
							m[key] = rightStr
							changed = true
						}
					}
				}
			}
		}
	}

	seen := make(map[string]struct{})
	adder := newLinkAdder(p, base, seen)

	for _, call := range collector.calls {
		name := p.calleeName(call.Callee)

		switch {
		case name == "fetch":
			p.addArgURL(call, 0, adder, vars, objects)
		case strings.HasPrefix(name, "axios"):
			if name == "axios" {
				p.addFromObjectPropArg(call, "url", adder, vars, objects)
			} else {
				p.addArgURL(call, 0, adder, vars, objects)
			}
		case name == "$.get" || name == "$.getJSON" || name == "$.post":
			p.addArgURL(call, 0, adder, vars, objects)
		case name == "$.ajax" || name == "jQuery.ajax":
			p.addFromObjectPropArg(call, "url", adder, vars, objects)
		case strings.HasSuffix(name, ".open"):
			if dotExpression, ok := call.Callee.(*ast.DotExpression); ok {
				if p.identIsNewXMLHttpRequest(dotExpression.Left) {
					p.addArgURL(call, 1, adder, vars, objects)
					continue
				}
			}

			if idName, ok := p.leftIdentNameOfDot(call.Callee); ok {
				if _, isXHR := collector.xhrVars[idName]; isXHR {
					p.addArgURL(call, 1, adder, vars, objects)
				}
			}
		}
	}

	for _, m := range urlRegex.FindAllString(src, -1) {
		adder.Add(m)
	}

	p.walk(parsedJS, &exprURLVisitor{
		p:       p,
		vars:    vars,
		objects: objects,
		adder:   adder,
	})

	links := make([]string, 0, len(seen))
	for u := range seen {
		links = append(links, u)
	}

	return links, nil
}

func (p *ParserBasic) addExprURL(ex ast.Expression, adder *linkAdder, vars map[string]string, objects map[string]map[string]string) {
	if ex == nil {
		return
	}

	if s, ok := p.evalStringExpr(ex, vars, objects); ok {
		adder.Add(s)
	}
}

func (p *ParserBasic) addArgURL(call *ast.CallExpression, idx int, adder *linkAdder, vars map[string]string, objects map[string]map[string]string) {
	if call == nil || idx < 0 || idx >= len(call.ArgumentList) {
		return
	}

	p.addExprURL(call.ArgumentList[idx], adder, vars, objects)
}

func (p *ParserBasic) addFromObjectPropArg(call *ast.CallExpression, prop string, adder *linkAdder, vars map[string]string, objects map[string]map[string]string) {
	if call == nil || len(call.ArgumentList) < 1 {
		return
	}

	if ol, ok := call.ArgumentList[0].(*ast.ObjectLiteral); ok {
		if urlVal := p.findObjectProp(ol, prop); urlVal != nil {
			p.addExprURL(urlVal, adder, vars, objects)
		}
	}
}

func (p *ParserBasic) findObjectProp(objectLiteral *ast.ObjectLiteral, key string) ast.Expression {
	props := p.objectProps(objectLiteral)
	for i := 0; i < len(props); i++ {
		if props[i].Key == key {
			return props[i].Val
		}
	}

	return nil
}

func (p *ParserBasic) calleeName(expression ast.Expression) string {
	switch n := expression.(type) {
	case *ast.Identifier:
		return n.Name.String()
	case *ast.DotExpression:
		leftName := p.calleeName(n.Left)
		method := n.Identifier.Name.String()

		if leftName == "" {
			return "." + method
		}
		return leftName + "." + method
	case *ast.BracketExpression:
		return ""
	case *ast.NewExpression:
		return p.calleeName(n.Callee)
	default:
		return ""
	}
}

func (p *ParserBasic) evalStringExpr(expression ast.Expression, vars map[string]string, objects map[string]map[string]string) (string, bool) {
	if expression == nil {
		return "", false
	}

	switch n := expression.(type) {
	case *ast.StringLiteral:
		return n.Value.String(), true
	case *ast.Identifier:
		if v, ok := vars[n.Name.String()]; ok {
			return v, true
		}
		return "", false
	case *ast.NumberLiteral:
		if s, ok := p.numberLiteralToString(n); ok {
			return s, true
		}
		return "", false
	case *ast.TemplateLiteral:
		parts, exprs := p.templateCookedPartsAndExprs(n)

		var b strings.Builder
		for i := 0; i < len(parts); i++ {
			b.WriteString(parts[i])
			if i < len(exprs) {
				s, ok := p.evalStringExpr(exprs[i], vars, objects)
				if !ok {
					return "", false
				}
				b.WriteString(s)
			}
		}
		return b.String(), true
	case *ast.BinaryExpression:
		if n.Operator == token.PLUS {
			left, lok := p.evalStringExpr(n.Left, vars, objects)
			right, rok := p.evalStringExpr(n.Right, vars, objects)
			if lok && rok {
				return left + right, true
			}
		}
		return "", false
	case *ast.DotExpression:
		if id, ok := n.Left.(*ast.Identifier); ok {
			if m := objects[id.Name.String()]; m != nil {
				prop := n.Identifier.Name.String()
				if v, ok := m[prop]; ok {
					return v, true
				}
			}
		}
		return "", false
	case *ast.BracketExpression:
		if id, ok := n.Left.(*ast.Identifier); ok {
			if key, ok2 := p.evalStringExpr(n.Member, vars, objects); ok2 {
				if m := objects[id.Name.String()]; m != nil {
					if v, ok := m[key]; ok {
						return v, true
					}
				}
			}
		}
		return "", false
	default:
		return "", false
	}
}

func (p *ParserBasic) walk(node ast.Node, v nodeVisitor) {
	if node == nil {
		return
	}
	v.visit(node)

	switch n := node.(type) {
	case *ast.Program:
		for _, s := range n.Body {
			if s != nil {
				p.walk(s, v)
			}
		}
		return
	case *ast.BlockStatement:
		for _, s := range n.List {
			if s != nil {
				p.walk(s, v)
			}
		}
		return
	case *ast.ExpressionStatement:
		if n.Expression != nil {
			p.walk(n.Expression, v)
		}
		return
	case *ast.VariableDeclaration:
		for _, ve := range n.List {
			if ve == nil {
				continue
			}
			rv := reflect.ValueOf(ve)
			if rv.IsValid() && rv.CanInterface() {
				if inner, ok := rv.Interface().(ast.Node); ok {
					p.walk(inner, v)
				}
			}
		}
		return
	case *ast.AssignExpression:
		if n.Left != nil {
			p.walk(n.Left, v)
		}
		if n.Right != nil {
			p.walk(n.Right, v)
		}
		return
	case *ast.BinaryExpression:
		if n.Left != nil {
			p.walk(n.Left, v)
		}
		if n.Right != nil {
			p.walk(n.Right, v)
		}
		return
	case *ast.CallExpression:
		if n.Callee != nil {
			p.walk(n.Callee, v)
		}
		for _, a := range n.ArgumentList {
			if a != nil {
				p.walk(a, v)
			}
		}
		return
	case *ast.DotExpression:
		if n.Left != nil {
			p.walk(n.Left, v)
		}
		p.walk(&n.Identifier, v)
		return
	case *ast.BracketExpression:
		if n.Left != nil {
			p.walk(n.Left, v)
		}
		if n.Member != nil {
			p.walk(n.Member, v)
		}
		return
	case *ast.FunctionLiteral:
		if n.ParameterList != nil {
			for _, id := range n.ParameterList.List {
				if id != nil {
					p.walk(id, v)
				}
			}
		}
		if n.Body != nil {
			p.walk(n.Body, v)
		}
		return
	}

	val := reflect.ValueOf(node)
	if val.Kind() == reflect.Ptr {
		val = val.Elem()
	}
	if val.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < val.NumField(); i++ {
		f := val.Field(i)
		if !f.CanInterface() {
			continue
		}

		switch f.Kind() {
		case reflect.Interface, reflect.Ptr:
			if f.IsNil() {
				continue
			}
			val := f.Interface()
			if n2, ok := val.(ast.Node); ok {
				p.walk(n2, v)
			}
		case reflect.Slice, reflect.Array:
			for j := 0; j < f.Len(); j++ {
				item := f.Index(j)
				if !item.CanInterface() {
					continue
				}
				if (item.Kind() == reflect.Interface || item.Kind() == reflect.Ptr) && item.IsNil() {
					continue
				}
				if n2, ok := item.Interface().(ast.Node); ok {
					p.walk(n2, v)
					continue
				}
				if item.CanAddr() {
					addr := item.Addr().Interface()
					if n2, ok := addr.(ast.Node); ok {
						p.walk(n2, v)
					}
				}
			}
		case reflect.Struct:
			if f.CanAddr() {
				addr := f.Addr().Interface()
				if n2, ok := addr.(ast.Node); ok {
					p.walk(n2, v)
				}
			}
		default:
		}
	}
}

func extractName(v reflect.Value) string {
	idExpr := getExprByFields(v, []string{"Id", "Left", "Name"})
	if id, ok := idExpr.(*ast.Identifier); ok {
		return id.Name.String()
	}
	return ""
}

func extractInit(v reflect.Value) ast.Expression {
	return getExprByFields(v, []string{"Init", "Initializer", "Right"})
}

func varDeclNameInit(raw interface{}) (string, ast.Expression) {
	reflectVal := reflect.ValueOf(raw)
	if !reflectVal.IsValid() {
		return "", nil
	}
	if reflectVal.Kind() == reflect.Ptr && reflectVal.IsNil() {
		return "", nil
	}
	reflectVal = derefValue(reflectVal)

	name := extractName(reflectVal)
	init := extractInit(reflectVal)
	return name, init
}

type objKV struct {
	Key string
	Val ast.Expression
}

func (p *ParserBasic) objectProps(ol *ast.ObjectLiteral) []objKV {
	if ol == nil {
		return nil
	}
	v := derefValue(reflect.ValueOf(ol))

	var list reflect.Value
	if f := v.FieldByName("Value"); f.IsValid() {
		list = f
	} else if f := v.FieldByName("Properties"); f.IsValid() {
		list = f
	}
	if !list.IsValid() || (list.Kind() != reflect.Slice && list.Kind() != reflect.Array) {
		return nil
	}

	out := make([]objKV, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		pv := derefValue(list.Index(i))
		keyF := pv.FieldByName("Key")

		key, ok := p.propKeyFromAny(keyF)
		if !ok {
			continue
		}

		valF := pv.FieldByName("Value")
		if !valF.IsValid() || (valF.Kind() == reflect.Interface && valF.IsNil()) {
			continue
		}

		if ex, ok := valF.Interface().(ast.Expression); ok {
			out = append(out, objKV{Key: key, Val: ex})
			continue
		}

		if valF.CanAddr() {
			if ex, ok := valF.Addr().Interface().(ast.Expression); ok {
				out = append(out, objKV{Key: key, Val: ex})
			}
		}
	}

	return out
}

func (p *ParserBasic) collectObjectLiteralStrings(ol *ast.ObjectLiteral, vars map[string]string, objects map[string]map[string]string) map[string]string {
	res := make(map[string]string)

	props := p.objectProps(ol)
	for i := 0; i < len(props); i++ {
		if s, ok := p.evalStringExpr(props[i].Val, vars, objects); ok {
			res[props[i].Key] = s
		}
	}

	return res
}

func (p *ParserBasic) propKeyFromAny(v reflect.Value) (string, bool) {
	if !v.IsValid() {
		return "", false
	}

	iv := v.Interface()
	switch k := iv.(type) {
	case *ast.Identifier:
		return k.Name.String(), true
	case *ast.StringLiteral:
		return k.Value.String(), true
	case *ast.NumberLiteral:
		return p.numberLitToString(k.Value)
	}

	if v.CanAddr() {
		if id, ok := v.Addr().Interface().(*ast.Identifier); ok {
			return id.Name.String(), true
		}
		if sl, ok := v.Addr().Interface().(*ast.StringLiteral); ok {
			return sl.Value.String(), true
		}
		if nl, ok := v.Addr().Interface().(*ast.NumberLiteral); ok {
			return p.numberLitToString(nl.Value)
		}
	}
	return "", false
}

func (p *ParserBasic) numberLiteralToString(n *ast.NumberLiteral) (string, bool) {
	if n == nil {
		return "", false
	}
	return p.numberLitToString(n.Value)
}

func (p *ParserBasic) numberLitToString(v interface{}) (string, bool) {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case string:
		return x, true
	default:
		return "", false
	}
}

func (p *ParserBasic) templateCookedPartsAndExprs(tl *ast.TemplateLiteral) (parts []string, exprs []ast.Expression) {
	if tl == nil {
		return nil, nil
	}

	v := derefValue(reflect.ValueOf(tl))

	var list reflect.Value
	if f := v.FieldByName("List"); f.IsValid() {
		list = f
	} else if f := v.FieldByName("Elements"); f.IsValid() {
		list = f
	}

	if list.IsValid() && (list.Kind() == reflect.Slice || list.Kind() == reflect.Array) {
		for i := 0; i < list.Len(); i++ {
			el := derefValue(list.Index(i))
			valF := el.FieldByName("Value")
			cooked := ""

			if valF.IsValid() {
				cv := derefValue(valF).FieldByName("Cooked")
				cooked = uniToString(cv)
			} else {
				cv := el.FieldByName("Cooked")
				cooked = uniToString(cv)
			}

			parts = append(parts, cooked)
		}
	}

	if ef := v.FieldByName("Expressions"); ef.IsValid() && (ef.Kind() == reflect.Slice || ef.Kind() == reflect.Array) {
		for i := 0; i < ef.Len(); i++ {
			if ex, ok := ef.Index(i).Interface().(ast.Expression); ok {
				exprs = append(exprs, ex)
			}
		}
	}

	return parts, exprs
}

func derefValue(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr) {
		if v.IsNil() {
			return v
		}

		v = v.Elem()
	}
	return v
}

func uniToString(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}

	if s, ok := v.Interface().(string); ok {
		return s
	}

	type stringer interface {
		String() string
	}

	if s, ok := v.Interface().(stringer); ok {
		return s.String()
	}

	return ""
}

func identIs(e ast.Expression, name string) bool {
	if id, ok := e.(*ast.Identifier); ok {
		return id.Name.String() == name
	}
	return false
}

func (p *ParserBasic) identIsNewXMLHttpRequest(e ast.Expression) bool {
	if ne, ok := e.(*ast.NewExpression); ok {
		return identIs(ne.Callee, "XMLHttpRequest")
	}
	if ce, ok := e.(*ast.CallExpression); ok {
		return identIs(ce.Callee, "XMLHttpRequest")
	}
	return false
}

func (p *ParserBasic) leftIdentNameOfDot(e ast.Expression) (string, bool) {
	if de, ok := e.(*ast.DotExpression); ok {
		if id, ok := de.Left.(*ast.Identifier); ok {
			return id.Name.String(), true
		}
	}
	return "", false
}

func mergeChanged(dst, src map[string]string) bool {
	changed := false
	for k, v := range src {
		if prev, ok := dst[k]; !ok || prev != v {
			dst[k] = v
			changed = true
		}
	}

	return changed
}

func getExprByFields(v reflect.Value, fieldNames []string) ast.Expression {
	for i := 0; i < len(fieldNames); i++ {
		f := v.FieldByName(fieldNames[i])
		if !f.IsValid() || (f.Kind() == reflect.Interface && f.IsNil()) {
			continue
		}

		if ex, ok := f.Interface().(ast.Expression); ok {
			return ex
		}

		if f.CanAddr() {
			if ex, ok := f.Addr().Interface().(ast.Expression); ok {
				return ex
			}
		}
	}
	return nil
}
