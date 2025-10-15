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

// TODO Я знаю, что этот код рефактора требует

type stringer interface {
	String() string
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

	varInits := make(map[string]ast.Expression)
	assigns := make([]*ast.AssignExpression, 0)
	calls := make([]*ast.CallExpression, 0)
	objLiterals := make(map[string]*ast.ObjectLiteral)
	xhrVars := make(map[string]struct{})

	p.walk(parsedJS, func(n ast.Node) {
		switch node := n.(type) {
		case *ast.VariableDeclaration:
			for _, raw := range node.List {
				name, init := p.varDeclNameInit(raw)
				if name == "" || init == nil {
					continue
				}
				varInits[name] = init
				switch typedInit := init.(type) {
				case *ast.ObjectLiteral:
					objLiterals[name] = typedInit
				case *ast.NewExpression:
					if p.identIs(typedInit.Callee, "XMLHttpRequest") {
						xhrVars[name] = struct{}{}
					}
				}
			}

		case *ast.AssignExpression:
			assigns = append(assigns, node)
			if id, ok := node.Left.(*ast.Identifier); ok {
				if ne, ok := node.Right.(*ast.NewExpression); ok && p.identIs(ne.Callee, "XMLHttpRequest") {
					xhrVars[id.Name.String()] = struct{}{}
				}
			}

		case *ast.CallExpression:
			calls = append(calls, node)
		}
	})

	vars := make(map[string]string)
	objects := make(map[string]map[string]string)

	for name, objectLiteral := range objLiterals {
		mapByName := objects[name]
		if mapByName == nil {
			mapByName = make(map[string]string)
			objects[name] = mapByName
		}
		p.forEachObjectProp(objectLiteral, func(key string, val ast.Expression) {
			if stringExpr, ok := p.evalStringExpr(val, vars, objects); ok {
				mapByName[key] = stringExpr
			}
		})
	}

	changed := true
	for changed {
		changed = false

		for name, expression := range varInits {
			if _, ok := vars[name]; ok {
				continue
			}
			if stringExpr, ok := p.evalStringExpr(expression, vars, objects); ok {
				vars[name] = stringExpr
				changed = true
			} else {
				if objectLiteral, ok := expression.(*ast.ObjectLiteral); ok {
					mapByName := objects[name]
					if mapByName == nil {
						mapByName = make(map[string]string)
						objects[name] = mapByName
					}
					p.forEachObjectProp(objectLiteral, func(k string, v ast.Expression) {
						if stringExpression, ok := p.evalStringExpr(v, vars, objects); ok {
							if prev, exists := mapByName[k]; !exists || prev != stringExpression {
								mapByName[k] = stringExpression
								changed = true
							}
						}
					})
				}
			}
		}

		for _, assignExpression := range assigns {
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
						mapByName := objects[idName]
						if mapByName == nil {
							mapByName = map[string]string{}
							objects[idName] = mapByName
						}
						if prev, ex := mapByName[key]; !ex || prev != rightStr {
							mapByName[key] = rightStr
							changed = true
						}
					}
				}
			}
		}
	}

	links := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(s string) {
		if !p.isURLCandidate(s) {
			return
		}
		if normalized := p.normalizeURL(s); normalized != "" {
			p.resolveAndAdd(normalized, seen, base)
		}
	}

	for _, call := range calls {
		name := p.calleeName(call.Callee)

		switch {
		case name == "fetch":
			if len(call.ArgumentList) >= 1 {
				if stringExpr, ok := p.evalStringExpr(call.ArgumentList[0], vars, objects); ok {
					add(stringExpr)
				}
			}

		case strings.HasPrefix(name, "axios"):
			if name == "axios" && len(call.ArgumentList) >= 1 {
				if objectLiteral, ok := call.ArgumentList[0].(*ast.ObjectLiteral); ok {
					if urlVal := p.findObjectProp(objectLiteral, "url"); urlVal != nil {
						if stringExpr, ok := p.evalStringExpr(urlVal, vars, objects); ok {
							add(stringExpr)
						}
					}
				}
			} else {
				if len(call.ArgumentList) >= 1 {
					if stringExpr, ok := p.evalStringExpr(call.ArgumentList[0], vars, objects); ok {
						add(stringExpr)
					}
				}
			}

		case name == "$.get" || name == "$.getJSON" || name == "$.post":
			if len(call.ArgumentList) >= 1 {
				if stringExpr, ok := p.evalStringExpr(call.ArgumentList[0], vars, objects); ok {
					add(stringExpr)
				}
			}

		case name == "$.ajax" || name == "jQuery.ajax":
			if len(call.ArgumentList) >= 1 {
				if objectLiteral, ok := call.ArgumentList[0].(*ast.ObjectLiteral); ok {
					if urlVal := p.findObjectProp(objectLiteral, "url"); urlVal != nil {
						if stringExpr, ok := p.evalStringExpr(urlVal, vars, objects); ok {
							add(stringExpr)
						}
					}
				}
			}

		case strings.HasSuffix(name, ".open"):
			if dotExpression, ok := call.Callee.(*ast.DotExpression); ok {
				if _, ok := dotExpression.Left.(*ast.NewExpression); ok && p.identIsNewXMLHttpRequest(dotExpression.Left) {
					if len(call.ArgumentList) >= 2 {
						if stringExpr, ok := p.evalStringExpr(call.ArgumentList[1], vars, objects); ok {
							add(stringExpr)
						}
						continue
					}
				}
			}

			if idName, ok := p.leftIdentNameOfDot(call.Callee); ok {
				if _, isXHR := xhrVars[idName]; isXHR && len(call.ArgumentList) >= 2 {
					if stringExpr, ok := p.evalStringExpr(call.ArgumentList[1], vars, objects); ok {
						add(stringExpr)
					}
				}
			}
		}
	}

	for _, m := range urlRegex.FindAllString(src, -1) {
		add(m)
	}

	for u := range seen {
		links = append(links, u)
	}

	return links, nil
}

func (p *ParserBasic) findObjectProp(objectLiteral *ast.ObjectLiteral, key string) ast.Expression {
	var found ast.Expression
	p.forEachObjectProp(objectLiteral, func(k string, v ast.Expression) {
		if found == nil && k == key {
			found = v
		}
	})
	return found
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

func (p *ParserBasic) walk(node ast.Node, fn func(ast.Node)) {
	if node == nil {
		return
	}
	fn(node)

	switch n := node.(type) {
	case *ast.Program:
		for _, s := range n.Body {
			if s != nil {
				p.walk(s, fn)
			}
		}
		return

	case *ast.BlockStatement:
		for _, s := range n.List {
			if s != nil {
				p.walk(s, fn)
			}
		}
		return

	case *ast.ExpressionStatement:
		if n.Expression != nil {
			p.walk(n.Expression, fn)
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
					p.walk(inner, fn)
				}
			}
		}
		return

	case *ast.AssignExpression:
		if n.Left != nil {
			p.walk(n.Left, fn)
		}
		if n.Right != nil {
			p.walk(n.Right, fn)
		}
		return

	case *ast.BinaryExpression:
		if n.Left != nil {
			p.walk(n.Left, fn)
		}
		if n.Right != nil {
			p.walk(n.Right, fn)
		}
		return

	case *ast.CallExpression:
		if n.Callee != nil {
			p.walk(n.Callee, fn)
		}
		for _, a := range n.ArgumentList {
			if a != nil {
				p.walk(a, fn)
			}
		}
		return

	case *ast.DotExpression:
		if n.Left != nil {
			p.walk(n.Left, fn)
		}
		p.walk(&n.Identifier, fn)
		return

	case *ast.BracketExpression:
		if n.Left != nil {
			p.walk(n.Left, fn)
		}
		if n.Member != nil {
			p.walk(n.Member, fn)
		}
		return

	case *ast.FunctionLiteral:
		if n.ParameterList != nil {
			for _, id := range n.ParameterList.List {
				if id != nil {
					p.walk(id, fn)
				}
			}
		}
		if n.Body != nil {
			p.walk(n.Body, fn)
		}
		return
	}

	v := reflect.ValueOf(node)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
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
				p.walk(n2, fn)
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
					p.walk(n2, fn)
					continue
				}
				// handle value-type identifiers by address
				if item.CanAddr() {
					addr := item.Addr().Interface()
					if n2, ok := addr.(ast.Node); ok {
						p.walk(n2, fn)
					}
				}
			}

		case reflect.Struct:
			if f.CanAddr() {
				addr := f.Addr().Interface()
				if n2, ok := addr.(ast.Node); ok {
					p.walk(n2, fn)
				}
			}
		default:

		}
	}
}

func (p *ParserBasic) varDeclNameInit(raw interface{}) (string, ast.Expression) {
	reflectVal := reflect.ValueOf(raw)

	if !reflectVal.IsValid() {
		return "", nil
	}

	if reflectVal.Kind() == reflect.Ptr && reflectVal.IsNil() {
		return "", nil
	}

	reflectVal = p.derefValue(reflectVal)

	// TODO refactor this shitty ass funny function later for the sake of humanity

	getExpr := func(v reflect.Value, fieldNames []string) ast.Expression {
		for _, fieldName := range fieldNames {
			f := v.FieldByName(fieldName)
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

	idExpr := getExpr(reflectVal, []string{"Id", "Left", "Name"})

	name := ""
	if id, ok := idExpr.(*ast.Identifier); ok {
		name = id.Name.String()
	}

	init := getExpr(reflectVal, []string{"Init", "Initializer", "Right"})

	return name, init
}

func (p *ParserBasic) forEachObjectProp(ol *ast.ObjectLiteral, fn func(key string, val ast.Expression)) {
	if ol == nil || fn == nil {
		return
	}
	v := p.derefValue(reflect.ValueOf(ol))

	var list reflect.Value
	if f := v.FieldByName("Value"); f.IsValid() {
		list = f
	} else if f := v.FieldByName("Properties"); f.IsValid() {
		list = f
	}
	if !list.IsValid() || (list.Kind() != reflect.Slice && list.Kind() != reflect.Array) {
		return
	}

	for i := 0; i < list.Len(); i++ {
		pv := p.derefValue(list.Index(i))

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
			fn(key, ex)
			continue
		}
		if valF.CanAddr() {
			if ex, ok := valF.Addr().Interface().(ast.Expression); ok {
				fn(key, ex)
			}
		}
	}
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

func (p *ParserBasic) isURLCandidate(s string) bool {
	s = strings.TrimSpace(s)
	return s != ""
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
	v := p.derefValue(reflect.ValueOf(tl))

	var list reflect.Value
	if f := v.FieldByName("List"); f.IsValid() {
		list = f
	} else if f := v.FieldByName("Elements"); f.IsValid() {
		list = f
	}
	if list.IsValid() && (list.Kind() == reflect.Slice || list.Kind() == reflect.Array) {
		for i := 0; i < list.Len(); i++ {
			el := p.derefValue(list.Index(i))
			valF := el.FieldByName("Value")
			cooked := ""
			if valF.IsValid() {
				cv := p.derefValue(valF).FieldByName("Cooked")
				cooked = p.uniToString(cv)
			} else {
				cv := el.FieldByName("Cooked")
				cooked = p.uniToString(cv)
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

func (p *ParserBasic) derefValue(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Ptr) {
		if v.IsNil() {
			return v
		}
		v = v.Elem()
	}
	return v
}

func (p *ParserBasic) uniToString(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	if s, ok := v.Interface().(string); ok {
		return s
	}

	if s, ok := v.Interface().(stringer); ok {
		return s.String()
	}
	return ""
}

func (p *ParserBasic) identIs(e ast.Expression, name string) bool {
	if id, ok := e.(*ast.Identifier); ok {
		return id.Name.String() == name
	}
	return false
}

func (p *ParserBasic) identIsNewXMLHttpRequest(e ast.Expression) bool {
	if ne, ok := e.(*ast.NewExpression); ok {
		return p.identIs(ne.Callee, "XMLHttpRequest")
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
