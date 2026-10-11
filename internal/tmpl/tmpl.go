// Package tmpl renders the subset of Tornado's template language that the
// webtiles HTML templates use, so the same template files serve both servers.
//
// Supported: {{ expr }}, {% raw expr %}, {% if expr %} / {% else %} / {% end %},
// {% include file %}, {% set name = expr %}, {% comment ... %} and {# ... #}.
// Expressions are a name, a string literal, `not expr`, static_url("..."),
// getattr(config, "name", default) and globals().get('name', default).
package tmpl

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// Env supplies the values a template can refer to.
type Env struct {
	// Values by name. A key may also be a whole expression, such as
	// "game.is_running()", which is looked up before anything else.
	Values map[string]any
	// Config is what getattr(config, "name", default) reads.
	Config    map[string]any
	StaticURL func(path string) string
	// Load returns the source of an included template.
	Load func(name string) (string, error)
}

type node struct {
	kind    string // text, expr, raw, if, include, set
	text    string // literal text, expression, or file name
	name    string // set: variable name
	yes, no []node // if: branches
}

// Render parses and renders src.
func Render(src string, env *Env) (string, error) {
	p := &parser{src: src}
	nodes, stop, err := p.parse()
	if err != nil {
		return "", err
	}
	if stop != "" {
		return "", fmt.Errorf("unexpected {%% %s %%}", stop)
	}
	env = &Env{Values: copyMap(env.Values), Config: env.Config, StaticURL: env.StaticURL, Load: env.Load}
	var out strings.Builder
	if err := render(nodes, env, &out, 0); err != nil {
		return "", err
	}
	return out.String(), nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type parser struct {
	src string
	pos int
}

// parse reads nodes until the end of input or an {% else %} / {% end %} tag,
// which it returns as stop.
func (p *parser) parse() (nodes []node, stop string, err error) {
	for p.pos < len(p.src) {
		rest := p.src[p.pos:]
		i := strings.IndexByte(rest, '{')
		if i < 0 || i+1 >= len(rest) {
			nodes = append(nodes, node{kind: "text", text: rest})
			p.pos = len(p.src)
			break
		}
		closer := map[byte]string{'{': "}}", '%': "%}", '#': "#}"}[rest[i+1]]
		if closer == "" {
			nodes = append(nodes, node{kind: "text", text: rest[:i+1]})
			p.pos += i + 1
			continue
		}
		if i > 0 {
			nodes = append(nodes, node{kind: "text", text: rest[:i]})
		}
		end := strings.Index(rest[i+2:], closer)
		if end < 0 {
			return nil, "", fmt.Errorf("unterminated %q", rest[i:i+2])
		}
		body := strings.TrimSpace(rest[i+2 : i+2+end])
		p.pos += i + 2 + end + 2

		switch rest[i+1] {
		case '#':
		case '{':
			nodes = append(nodes, node{kind: "expr", text: body})
		case '%':
			op, arg, _ := strings.Cut(body, " ")
			arg = strings.TrimSpace(arg)
			switch op {
			case "end", "else":
				return nodes, op, nil
			case "comment":
			case "raw":
				nodes = append(nodes, node{kind: "raw", text: arg})
			case "include":
				nodes = append(nodes, node{kind: "include", text: strings.Trim(arg, `"'`)})
			case "set":
				name, expr, ok := strings.Cut(arg, "=")
				if !ok {
					return nil, "", fmt.Errorf("malformed {%% set %s %%}", arg)
				}
				nodes = append(nodes, node{kind: "set", name: strings.TrimSpace(name), text: strings.TrimSpace(expr)})
			case "if":
				n := node{kind: "if", text: arg}
				var s string
				if n.yes, s, err = p.parse(); err != nil {
					return nil, "", err
				}
				if s == "else" {
					if n.no, s, err = p.parse(); err != nil {
						return nil, "", err
					}
				}
				if s != "end" {
					return nil, "", fmt.Errorf("{%% if %s %%} without {%% end %%}", arg)
				}
				nodes = append(nodes, n)
			default:
				return nil, "", fmt.Errorf("unsupported template tag {%% %s %%}", op)
			}
		}
	}
	return nodes, "", nil
}

func render(nodes []node, env *Env, out *strings.Builder, depth int) error {
	for _, n := range nodes {
		switch n.kind {
		case "text":
			out.WriteString(n.text)
		case "expr", "raw":
			v, err := eval(n.text, env)
			if err != nil {
				return err
			}
			s := toString(v)
			if n.kind == "expr" {
				s = html.EscapeString(s)
			}
			out.WriteString(s)
		case "set":
			v, err := eval(n.text, env)
			if err != nil {
				return err
			}
			env.Values[n.name] = v
		case "if":
			v, err := eval(n.text, env)
			if err != nil {
				return err
			}
			branch := n.no
			if truthy(v) {
				branch = n.yes
			}
			if err := render(branch, env, out, depth); err != nil {
				return err
			}
		case "include":
			if depth > 8 || env.Load == nil {
				return fmt.Errorf("cannot include %s", n.text)
			}
			src, err := env.Load(n.text)
			if err != nil {
				return err
			}
			p := &parser{src: src}
			sub, stop, err := p.parse()
			if err != nil || stop != "" {
				return fmt.Errorf("in included template %s: %v %s", n.text, err, stop)
			}
			if err := render(sub, env, out, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	stringRe  = regexp.MustCompile(`^(?:"([^"\\]*)"|'([^'\\]*)')$`)
	staticRe  = regexp.MustCompile(`^static_url\(\s*(?:"([^"\\]*)"|'([^'\\]*)')\s*\)$`)
	getattrRe = regexp.MustCompile(`^getattr\(\s*config\s*,\s*["']([A-Za-z0-9_]+)["']\s*,\s*(.+)\)$`)
	globalsRe = regexp.MustCompile(`^globals\(\)\.get\(\s*["']([A-Za-z0-9_]+)["']\s*,\s*(.+)\)$`)
)

func eval(expr string, env *Env) (any, error) {
	expr = strings.TrimSpace(expr)
	if v, ok := env.Values[expr]; ok {
		return v, nil
	}
	if rest, ok := strings.CutPrefix(expr, "not "); ok {
		v, err := eval(rest, env)
		return !truthy(v), err
	}
	switch expr {
	case "True":
		return true, nil
	case "False":
		return false, nil
	case "None":
		return nil, nil
	}
	if m := stringRe.FindStringSubmatch(expr); m != nil {
		return m[1] + m[2], nil
	}
	if m := staticRe.FindStringSubmatch(expr); m != nil {
		return env.StaticURL(m[1] + m[2]), nil
	}
	if m := getattrRe.FindStringSubmatch(expr); m != nil {
		if v, ok := env.Config[m[1]]; ok {
			return v, nil
		}
		return eval(m[2], env)
	}
	if m := globalsRe.FindStringSubmatch(expr); m != nil {
		if v, ok := env.Values[m[1]]; ok {
			return v, nil
		}
		return eval(m[2], env)
	}
	if nameRe.MatchString(expr) {
		return nil, nil // an unset name
	}
	return nil, fmt.Errorf("unsupported template expression: %s", expr)
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case int:
		return x != 0
	}
	return true
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
