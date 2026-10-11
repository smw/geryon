package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Game is one game definition (or template). Lookups fall through to the
// game's template, then that template's template, and so on.
type Game struct {
	ID       string
	keys     []string
	store    map[string]any
	template string // "" for the root templates
	set      *GameSet
}

// GameSet is the full set of loaded games, in lobby order.
type GameSet struct {
	Games     []*Game
	byID      map[string]*Game
	templates map[string]*Game

	// Game id -> crawl game mode name ("normal", "sprint", ...); missing if the
	// binary could not report its modes or save info is off for it.
	Modes map[string]string
	// Binary key -> ids of the games that run that binary, in lobby order.
	BinaryGames map[string][]string
	BinaryKeys  []string
}

func (s *GameSet) Get(id string) *Game { return s.byID[id] }

func (g *Game) parent() *Game {
	if g.template == "" || g.set == nil {
		return nil
	}
	return g.set.templates[g.template]
}

func (g *Game) lookup(key string) (any, bool) {
	for cur, depth := g, 0; cur != nil && depth < 32; cur, depth = cur.parent(), depth+1 {
		if v, ok := cur.store[key]; ok {
			return v, true
		}
	}
	return nil, false
}

// Has reports whether key is set on the game or any of its templates.
func (g *Game) Has(key string) bool {
	_, ok := g.lookup(key)
	return ok
}

// Keys lists every key visible on the game: its own first, then inherited.
func (g *Game) Keys() []string {
	var out []string
	seen := map[string]bool{}
	for cur, depth := g, 0; cur != nil && depth < 32; cur, depth = cur.parent(), depth+1 {
		for _, k := range cur.keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// Format expands %n (username, when one is given), %v, %V and %r (version).
func (g *Game) Format(s, username string) string { return g.format(s, username) }

func (g *Game) format(s, username string) string {
	if username != "" {
		s = strings.ReplaceAll(s, "%n", username)
	}
	v, _ := g.lookup("version")
	version, ok := v.(string)
	if !ok {
		return s
	}
	s = strings.ReplaceAll(s, "%v", version)
	if strings.Contains(s, "%V") {
		capital := version
		if capital != "" {
			capital = strings.ToUpper(capital[:1]) + strings.ToLower(capital[1:])
		}
		s = strings.ReplaceAll(s, "%V", capital)
	}
	if strings.Contains(s, "%r") {
		r := version
		if i := strings.Index(version, "0."); i >= 0 {
			r = version[i+2:]
		}
		s = strings.ReplaceAll(s, "%r", r)
	}
	return s
}

// Str returns a string option with its placeholders expanded, or "" if unset.
// An empty username leaves %n alone.
func (g *Game) Str(key, username string) string {
	v, _ := g.lookup(key)
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return g.format(s, username)
}

// List returns a list-of-strings option with placeholders expanded.
func (g *Game) List(key, username string) []string {
	v, _ := g.lookup(key)
	l, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, e := range l {
		if s, ok := e.(string); ok {
			out = append(out, g.format(s, username))
		}
	}
	return out
}

// Map returns a string-to-string option (env) with placeholders expanded.
func (g *Game) Map(key, username string) map[string]string {
	v, _ := g.lookup(key)
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, e := range m {
		if s, ok := e.(string); ok {
			out[k] = g.format(s, username)
		}
	}
	return out
}

// Bool returns a boolean option, or def if unset.
func (g *Game) Bool(key string, def bool) bool {
	v, _ := g.lookup(key)
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

// CallBase is the command that starts the game's crawl binary, without any
// per-player arguments.
func (g *Game) CallBase() []string {
	return append([]string{g.Str("crawl_binary", "")}, g.List("pre_options", "")...)
}

// BinaryKey identifies the crawl binary a game runs. pre_options is part of it
// because some setups select the version through a wrapper script's arguments.
func (g *Game) BinaryKey() string { return strings.Join(g.CallBase(), " ") }

// MorgueURL is the morgue_url option; "" when unset. The stock games.d file
// spells "unset" as a bare YAML `None`, which YAML reads as a string.
func (g *Game) MorgueURL(username string) string {
	u := g.Str("morgue_url", username)
	if u == "None" {
		return ""
	}
	return u
}

// DGLLessGame is the single game used when dgl_mode is off.
func DGLLessGame() *Game {
	keys := []string{"name", "ttyrec_path", "inprogress_path", "socket_path", "client_path"}
	return &Game{ID: "dcss-webtiles", keys: keys, store: map[string]any{
		"name":            "DCSS",
		"ttyrec_path":     "./",
		"inprogress_path": "./",
		"socket_path":     "./",
		"client_path":     "./webserver/game_data",
	}}
}

type gameDefs struct {
	templates, games []*Game
	source           string
}

func newGame(n *yaml.Node, id string) (*Game, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("game definition must be a map")
	}
	g := &Game{ID: id, store: map[string]any{}}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		var val any
		if err := n.Content[i+1].Decode(&val); err != nil {
			return nil, err
		}
		g.keys = append(g.keys, key)
		g.store[key] = val
	}
	if g.ID == "" {
		g.ID, _ = g.store["id"].(string)
	}
	if g.ID == "" {
		return nil, fmt.Errorf("definition missing id")
	}
	if _, ok := g.store["id"]; !ok {
		g.keys = append(g.keys, "id")
	}
	g.store["id"] = g.ID
	return g, nil
}

// parseDefs reads a `templates`/`games` value, which is either a list of maps
// each carrying an id, or a map from id to definition.
func parseDefs(n *yaml.Node, kind, source string) []*Game {
	var out []*Game
	seen := map[string]bool{}
	add := func(def *yaml.Node, id string) {
		g, err := newGame(def, id)
		if err != nil {
			log.Printf("Skipping a definition in %s of %s: %v", kind, source, err)
			return
		}
		if seen[g.ID] {
			log.Printf("Duplicate id %s in %s (source %s), skipping.", g.ID, kind, source)
			return
		}
		seen[g.ID] = true
		out = append(out, g)
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, def := range n.Content {
			add(def, "")
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			add(n.Content[i+1], n.Content[i].Value)
		}
	}
	return out
}

func parseGamesFile(data []byte, source string) gameDefs {
	var doc struct {
		Templates yaml.Node `yaml:"templates"`
		Games     yaml.Node `yaml:"games"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		log.Printf("Failed to load games from %s, skipping (parse failure: %v).", source, err)
		return gameDefs{source: source}
	}
	return gameDefs{
		templates: parseDefs(&doc.Templates, "templates", source),
		games:     parseDefs(&doc.Games, "games", source),
		source:    source,
	}
}

// LoadGames builds the game set from config.yml and the games.d/*.yaml files
// in the server path. It reads the files afresh each time, so calling it again
// picks up changes.
func LoadGames(c *Config) (*GameSet, error) {
	set := &GameSet{byID: map[string]*Game{}, templates: map[string]*Game{}}

	merge := func(defs gameDefs) error {
		if n := len(defs.templates); n > 0 {
			log.Printf("Reading %d templates from %s", n, defs.source)
		}
		if n := len(defs.games); n > 0 {
			log.Printf("Reading %d games from %s", n, defs.source)
		}
		for _, t := range defs.templates {
			if t.ID == "base" {
				return fmt.Errorf("cannot redefine reserved template name `base`")
			}
			if _, dup := set.templates[t.ID]; dup {
				log.Printf("Duplicate id %s in templates (source %s), skipping.", t.ID, defs.source)
				continue
			}
			set.templates[t.ID] = t
		}
		for _, g := range defs.games {
			if _, dup := set.byID[g.ID]; dup {
				log.Printf("Duplicate id %s in games (source %s), skipping.", g.ID, defs.source)
				continue
			}
			set.byID[g.ID] = g
			set.Games = append(set.Games, g)
		}
		return nil
	}

	fromConfig := gameDefs{
		templates: parseDefs(&c.Templates, "templates", "config.yml"),
		games:     parseDefs(&c.Games, "games", "config.yml"),
		source:    "config.yml",
	}
	if err := merge(fromConfig); err != nil {
		return nil, err
	}

	useDir := len(fromConfig.games) == 0
	if c.UseGameYAML != nil {
		useDir = *c.UseGameYAML
	}
	if useDir {
		where := filepath.Join(c.ServerPath, "games.d")
		dir := os.DirFS(where)
		entries, err := fs.ReadDir(dir, ".")
		if err != nil {
			log.Printf("Skipping unreadable game configuration directory %s: %v", where, err)
		}
		for _, e := range entries { // ReadDir sorts by name
			if !strings.HasSuffix(e.Name(), ".yaml") {
				log.Printf("Ignoring non-.yaml file %s/%s", where, e.Name())
				continue
			}
			data, err := fs.ReadFile(dir, e.Name())
			if err != nil {
				log.Printf("Couldn't read file %s/%s: %v", where, e.Name(), err)
				continue
			}
			if err := merge(parseGamesFile(data, where+"/"+e.Name())); err != nil {
				return nil, err
			}
		}
	} else {
		log.Printf("Skipping game definitions in `games.d/` based on config")
	}

	// Wire up template inheritance. Everything inherits from `default` unless it
	// names another template; `default` and `base` inherit from nothing.
	for _, name := range []string{"base", "default"} {
		if set.templates[name] == nil {
			set.templates[name] = &Game{ID: name, store: map[string]any{}}
		}
	}
	link := func(g *Game, isTemplate bool) error {
		g.set = set
		if isTemplate && (g.ID == "base" || g.ID == "default") {
			return nil
		}
		g.template = "default"
		if t, ok := g.store["template"].(string); ok {
			g.template = t
		}
		delete(g.store, "template")
		if set.templates[g.template] == nil {
			return fmt.Errorf("game %s uses unknown template %s", g.ID, g.template)
		}
		return nil
	}
	for _, t := range set.templates {
		if err := link(t, true); err != nil {
			return nil, err
		}
	}
	for _, g := range set.Games {
		if err := link(g, false); err != nil {
			return nil, err
		}
	}
	for _, t := range set.templates {
		seen := map[string]bool{}
		for cur := t; cur != nil; cur = cur.parent() {
			if seen[cur.ID] {
				return nil, fmt.Errorf("loop in template dependencies from %s: %s", t.ID, cur.ID)
			}
			seen[cur.ID] = true
		}
	}

	if len(set.Games) == 0 {
		return nil, fmt.Errorf("no games defined")
	}
	ok := true
	for _, g := range set.Games {
		ok = validateGame(g) && ok
	}
	if !ok {
		return nil, fmt.Errorf("errors in game data")
	}

	set.collectModes()
	for _, g := range set.Games {
		k := g.BinaryKey()
		if _, seen := set.BinaryGames[k]; !seen {
			set.BinaryKeys = append(set.BinaryKeys, k)
		}
		set.BinaryGames[k] = append(set.BinaryGames[k], g.ID)
	}
	return set, nil
}

func validateGame(g *Game) bool {
	ok := true
	for _, key := range []string{"name", "crawl_binary", "rcfile_path", "macro_path",
		"morgue_path", "inprogress_path", "ttyrec_path", "socket_path"} {
		if !g.Has(key) {
			log.Printf("Missing required property '%s' in game '%s'", key, g.ID)
			ok = false
		}
	}
	for _, key := range g.Keys() {
		v, _ := g.lookup(key)
		s, isStr := v.(string)
		if !isStr {
			continue
		}
		if !g.Has("version") &&
			(strings.Contains(s, "%v") || strings.Contains(s, "%V") || strings.Contains(s, "%r")) {
			log.Printf("Game '%s', key '%s': version used in templating but not set: %s", g.ID, key, s)
			ok = false
		}
		switch key {
		case "crawl_binary", "socket_path":
			if strings.Contains(s, "%n") {
				log.Printf("'%%n' will not be expanded in %s (game %s): '%s'", key, g.ID, s)
				ok = false
			}
		}
	}
	if !ok {
		return false
	}
	if bin := g.Str("crawl_binary", ""); !exists(bin) {
		log.Printf("Crawl executable for %s ('%s') doesn't exist!", g.ID, bin)
		ok = false
	}
	if cp := g.Str("client_path", ""); g.Has("client_path") && !exists(cp) {
		log.Printf("Client data path for %s ('%s') doesn't exist; using the client built into the server.", g.ID, cp)
	}
	return ok
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// orderedModes is the output of `crawl -gametypes-json`: mode name -> the
// command line option that selects it, in the order crawl printed them.
type orderedModes struct {
	names   []string
	options map[string]string
}

func parseModes(data []byte) (*orderedModes, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	m := &orderedModes{options: map[string]string{}}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v string
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		name, _ := k.(string)
		m.names = append(m.names, name)
		m.options[name] = v
	}
	return m, nil
}

// collectModes asks each crawl binary which game modes it supports, and works
// out which mode each configured game is. Only binaries with a game that shows
// save info are asked.
func (s *GameSet) collectModes() {
	s.Modes = map[string]string{}
	s.BinaryGames = map[string][]string{}
	binaries := map[string]*orderedModes{}
	for _, g := range s.Games {
		key := g.BinaryKey()
		if !g.Bool("show_save_info", false) || binaries[key] != nil {
			continue
		}
		call := append(g.CallBase(), "-gametypes-json", "dummy")
		out, err := exec.Command(call[0], call[1:]...).Output()
		if err != nil {
			continue
		}
		modes, err := parseModes(out)
		if err != nil {
			log.Printf("JSON error with game mode output '%s'", out)
			continue
		}
		binaries[key] = modes
	}

	for _, g := range s.Games {
		modes := binaries[g.BinaryKey()]
		if modes == nil {
			continue
		}
		options := g.List("options", "")
		found := false
		for _, mode := range modes.names {
			for _, opt := range options {
				if opt == modes.options[mode] {
					s.Modes[g.ID] = mode
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			for _, mode := range modes.names {
				if modes.options[mode] == "" {
					s.Modes[g.ID] = mode
					break
				}
			}
		}
	}
}
