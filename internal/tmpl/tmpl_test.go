package tmpl

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	env := &Env{
		Values:    map[string]any{"username": "A&B", "game.is_running()": true, "sep": "<br>"},
		Config:    map[string]any{"allow_password_reset": false},
		StaticURL: func(p string) string { return "/static/" + p + "?v=1" },
		Load: func(name string) (string, error) {
			return "[{% if username %}Hello, {{ username }}!{% end %}]", nil
		},
	}
	cases := []struct{ src, want string }{
		{`plain { text } {`, `plain { text } {`},
		{`{{ username }}|{% raw sep %}|{{ sep }}`, `A&amp;B|<br>|&lt;br&gt;`},
		{`<script src="{{ static_url("a/b.js") }}">`, `<script src="/static/a/b.js?v=1">`},
		{`{% if getattr(config, "allow_password_reset", False) %}yes{% else %}no{% end %}`, `no`},
		{`{% if getattr(config, "unset_option", True) %}yes{% end %}`, `yes`},
		{`{% if game.is_running() %}saved{% end %}`, `saved`},
		{`{% if missing %}a{% else %}{% if not missing %}b{% end %}{% end %}`, `b`},
		{`{% set v = globals().get('game_version', '') %}"{{ v }}"`, `""`},
		{`{# gone #}{% comment gone too %}x{% include banner.html %}`, `x[Hello, A&amp;B!]`},
	}
	for _, c := range cases {
		got, err := Render(c.src, env)
		if err != nil {
			t.Errorf("Render(%q): %v", c.src, err)
		} else if got != c.want {
			t.Errorf("Render(%q) = %q, want %q", c.src, got, c.want)
		}
	}
	for _, bad := range []string{`{% for x in y %}{% end %}`, `{% if a %}`, `{{ a + b }}`, `{% end %}`} {
		if out, err := Render(bad, env); err == nil {
			t.Errorf("Render(%q) = %q, want an error", bad, out)
		} else if strings.Contains(err.Error(), "panic") {
			t.Errorf("Render(%q): %v", bad, err)
		}
	}
}
