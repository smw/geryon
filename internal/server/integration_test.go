package server

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/smw/geryon/internal/config"
	"github.com/smw/geryon/internal/userdb"
)

// These tests run real crawl games through the server. They need a webtiles
// build of crawl (`make WEBTILES=y`): set GERYON_CRAWL_SOURCE to its
// crawl-ref/source directory. Without that they are skipped.

type testServer struct {
	t      *testing.T
	dir    string
	addr   string
	srv    *Server
	cancel context.CancelFunc
	done   chan error

	stopOnce sync.Once
}

func crawlSource(t *testing.T) string {
	source := os.Getenv("GERYON_CRAWL_SOURCE")
	if source == "" {
		t.Skip("GERYON_CRAWL_SOURCE is not set")
	}
	source, err := filepath.Abs(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(source, "crawl")); err != nil {
		t.Fatal("no crawl binary at " + source + "; build with `make WEBTILES=y`")
	}
	return source
}

// writeGameDef adds a game to the server directory's games.d.
func writeGameDef(t *testing.T, dir, source, id, name string) {
	t.Helper()
	def := fmt.Sprintf(`games:
  - id: %[3]s
    name: %[4]s
    crawl_binary: %[2]s/crawl
    rcfile_path: %[1]s/rcs/
    macro_path: %[1]s/rcs/
    morgue_path: %[1]s/rcs/morgue/%%n
    socket_path: %[1]s/rcs
    inprogress_path: %[1]s/rcs/running
    ttyrec_path: %[1]s/rcs/ttyrecs/%%n
    client_path: %[2]s/webserver/game_data/
    cwd: %[1]s/work
    show_save_info: true
`, dir, source, id, name)
	if err := os.WriteFile(filepath.Join(dir, "games.d", id+".yaml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
}

func startTestServer(t *testing.T, tweak func(*config.Config)) *testServer {
	source := crawlSource(t)
	dir := t.TempDir()

	// Crawl keeps saves under its working directory; give it one of its own.
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The init program does what util/webtiles-init-player.sh does, and gives
	// the player an rc file that skips character creation.
	initScript := filepath.Join(dir, "init-player.sh")
	script := fmt.Sprintf(`#!/bin/sh
mkdir -p %[1]s/rcs/running %[1]s/rcs/ttyrecs/$1 %[1]s/rcs/morgue/$1
if [ ! -f %[1]s/rcs/$1.rc ]; then
    printf 'species = Minotaur\nbackground = Berserker\nweapon = hand axe\n' > %[1]s/rcs/$1.rc
fi
`, dir)
	if err := os.WriteFile(initScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	if err := os.Mkdir(filepath.Join(dir, "games.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeGameDef(t, dir, source, "test", "Test game")

	cfg := config.Defaults()
	cfg.ServerPath = dir
	cfg.BindAddress, cfg.BindPort = "127.0.0.1", port
	cfg.PasswordDB = filepath.Join(dir, "passwd.db3")
	cfg.SettingsDB = filepath.Join(dir, "user_settings.db3")
	cfg.StaticPath = filepath.Join(source, "webserver", "static")
	cfg.TemplatePath = filepath.Join(source, "webserver", "templates")
	cfg.InitPlayerProgram = initScript
	cfg.ServerSocketPath = dir
	cfg.CryptAlgorithm = "6"
	cfg.KillTimeout = 5
	if tweak != nil {
		tweak(cfg)
	}

	gameSet, err := config.LoadGames(cfg)
	if err != nil {
		t.Fatal(err)
	}
	db, err := userdb.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, gameSet, db)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ts := &testServer{t: t, dir: dir, addr: fmt.Sprintf("127.0.0.1:%d", port), srv: srv,
		cancel: cancel, done: make(chan error, 1)}
	go func() { ts.done <- srv.Run(ctx) }()
	for i := 0; ; i++ {
		conn, err := net.Dial("tcp", ts.addr)
		if err == nil {
			conn.Close()
			break
		}
		if i > 100 {
			t.Fatal("server did not start listening")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		ts.stop()
		db.Close()
	})
	return ts
}

// stop shuts the server down and waits for it. Safe to call more than once.
func (ts *testServer) stop() {
	ts.stopOnce.Do(func() {
		ts.cancel()
		select {
		case <-ts.done:
		case <-time.After(40 * time.Second):
			ts.t.Error("server did not shut down")
		}
	})
}

// client is a browser stand-in.
type client struct {
	t        *testing.T
	name     string
	ws       *websocket.Conn
	sendMu   sync.Mutex
	msgs     chan map[string]any
	frames   [][]byte // compressed frames as received, when deflate is on
	inflated int      // bytes of the decompressed stream already consumed
}

func (ts *testServer) connect(name string, deflate bool) *client {
	dialer := websocket.Dialer{}
	if !deflate {
		dialer.Subprotocols = []string{"no-compression"}
	}
	ws, _, err := dialer.Dial("ws://"+ts.addr+"/socket", nil)
	if err != nil {
		ts.t.Fatal(err)
	}
	c := &client{t: ts.t, name: name, ws: ws, msgs: make(chan map[string]any, 4096)}
	go c.readLoop(deflate)
	ts.t.Cleanup(func() { ws.Close() })
	return c
}

// inflate decodes a frame the way the browser does: each frame continues one
// deflate stream, and has lost the 00 00 FF FF that ended it.
func (c *client) inflate(frame []byte) ([]byte, error) {
	c.frames = append(c.frames, frame)
	var stream bytes.Buffer
	for _, f := range c.frames {
		stream.Write(f)
		stream.Write([]byte{0, 0, 0xff, 0xff})
	}
	all, err := io.ReadAll(flate.NewReader(&stream))
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	out := all[c.inflated:]
	c.inflated = len(all)
	return out, nil
}

func (c *client) readLoop(deflate bool) {
	defer close(c.msgs)
	for {
		kind, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if deflate != (kind == websocket.BinaryMessage) {
			c.t.Errorf("%s: frame type %d does not match compression setting %v", c.name, kind, deflate)
			return
		}
		if deflate {
			if data, err = c.inflate(data); err != nil {
				c.t.Errorf("%s: cannot inflate frame: %v", c.name, err)
				return
			}
		}
		var batch struct {
			Msgs []map[string]any `json:"msgs"`
		}
		if err := json.Unmarshal(data, &batch); err != nil {
			c.t.Errorf("%s: invalid batch: %v\n%.300s", c.name, err, data)
			return
		}
		for _, m := range batch.Msgs {
			if m["msg"] == "ping" {
				c.send(map[string]any{"msg": "pong"})
				continue
			}
			c.msgs <- m
		}
	}
}

func (c *client) send(v map[string]any) {
	data, _ := json.Marshal(v)
	c.sendMu.Lock() // the read loop answers pings
	defer c.sendMu.Unlock()
	if err := c.ws.WriteMessage(websocket.TextMessage, data); err != nil {
		c.t.Logf("%s: send failed: %v", c.name, err)
	}
}

// expect waits for a message of the given type and returns it, skipping
// others.
func (c *client) expect(msgType string) map[string]any {
	c.t.Helper()
	return c.expectMatch(msgType, func(map[string]any) bool { return true })
}

func (c *client) expectMatch(msgType string, match func(map[string]any) bool) map[string]any {
	c.t.Helper()
	var skipped []string
	deadline := time.After(30 * time.Second)
	for {
		select {
		case m, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("%s: connection closed while waiting for '%s' (saw %v)", c.name, msgType, skipped)
			}
			if m["msg"] == msgType && match(m) {
				return m
			}
			skipped = append(skipped, fmt.Sprint(m["msg"]))
		case <-deadline:
			c.t.Fatalf("%s: timed out waiting for '%s' (saw %v)", c.name, msgType, skipped)
		}
	}
}

// expectClosed waits for the server to close the connection.
func (c *client) expectClosed() {
	c.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case _, ok := <-c.msgs:
			if !ok {
				return
			}
		case <-deadline:
			c.t.Fatalf("%s: connection was not closed", c.name)
		}
	}
}

func (c *client) register(username, password string) {
	c.t.Helper()
	c.send(map[string]any{"msg": "register", "username": username, "password": password, "email": ""})
	if m := c.expect("login_success"); m["username"] != username {
		c.t.Fatalf("login_success for %v, want %s", m["username"], username)
	}
}

func (c *client) key(code int) { c.send(map[string]any{"msg": "key", "keycode": code}) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func dirEntries(t *testing.T, dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestHTTP(t *testing.T) {
	ts := startTestServer(t, nil)
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get("http://" + ts.addr + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, page := get("/")
	if resp.StatusCode != 200 {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	for _, want := range []string{
		`var socket_server = "ws://` + ts.addr + `/socket";`,
		`/static/scripts/contrib/require.js?v=`,
		`Welcome to WebTiles!`, // banner.html, included
		`id="footer"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index page lacks %q", want)
		}
	}
	if strings.Contains(page, "{%") || strings.Contains(page, "{{") {
		t.Errorf("index page has unrendered template syntax")
	}
	if strings.Contains(page, "forgot_link") {
		t.Errorf("index page offers password reset, which the server does not implement")
	}

	// A versioned link may be cached forever; a bare one is revalidated.
	i := strings.Index(page, "/static/style.css?v=")
	if i < 0 {
		t.Fatal("no versioned style.css link")
	}
	link := page[i : i+strings.IndexByte(page[i:], '"')]
	resp, css := get(link)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "max-age=") || len(css) == 0 {
		t.Errorf("GET %s: status %d, Cache-Control %q", link, resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	resp, _ = get("/static/scripts/app.js")
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "" || etag == "" {
		t.Errorf("GET app.js: status %d, Cache-Control %q, ETag %q", resp.StatusCode, resp.Header.Get("Cache-Control"), etag)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("app.js Content-Type = %q", ct)
	}
	req, _ := http.NewRequest("GET", "http://"+ts.addr+"/static/scripts/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET app.js: %v, status %v", err, resp.StatusCode)
	}

	for _, path := range []string{"/static/../../config.yml", "/static/nonexistent.js", "/static/scripts/",
		"/gamedata/0000/game.js", "/nope"} {
		if resp, _ := get(path); resp.StatusCode != 404 {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	if resp, body := get("/status/lobby/"); resp.StatusCode != 200 || strings.TrimSpace(body) != "[]" {
		t.Errorf("GET /status/lobby/ = %d %q", resp.StatusCode, body)
	}
}

func TestAccounts(t *testing.T) {
	ts := startTestServer(t, nil)

	c := ts.connect("first", false)
	c.expect("lobby_complete")
	c.send(map[string]any{"msg": "login", "username": "nobody", "password": "x"})
	c.expect("login_fail")
	c.send(map[string]any{"msg": "register", "username": "x", "password": "pw", "email": ""})
	if m := c.expect("register_fail"); m["reason"] != "Account creation failed." {
		t.Errorf("short name: reason %v", m["reason"])
	}
	c.register("Alice", "secret pw!%&")
	links := c.expectMatch("set_game_links", func(m map[string]any) bool { return true })
	if content := fmt.Sprint(links["content"]); !strings.Contains(content, `<a href="#play-test">Test game</a>`) ||
		!strings.Contains(content, `data-game_id="test"`) {
		t.Errorf("game links: %s", content)
	}
	c.send(map[string]any{"msg": "set_login_cookie"})
	cookie := fmt.Sprint(c.expect("login_cookie")["cookie"])

	// rc files round-trip.
	c.send(map[string]any{"msg": "set_rc", "game_id": "test", "contents": "species = Minotaur\nbackground = Berserker\nweapon = hand axe\n# edited\n"})
	c.send(map[string]any{"msg": "get_rc", "game_id": "test"})
	if rc := fmt.Sprint(c.expect("rcfile_contents")["contents"]); !strings.HasSuffix(rc, "# edited\n") {
		t.Errorf("rc contents: %q", rc)
	}

	// Logging in: wrong password, any-case username, and the cookie (once).
	c2 := ts.connect("second", false)
	c2.send(map[string]any{"msg": "login", "username": "alice", "password": "wrong"})
	c2.expect("login_fail")
	c2.send(map[string]any{"msg": "login", "username": "alice", "password": "secret pw!%&"})
	if m := c2.expect("login_success"); m["username"] != "Alice" || m["admin"] != false {
		t.Errorf("login_success: %v", m)
	}
	c3 := ts.connect("third", false)
	c3.send(map[string]any{"msg": "token_login", "cookie": cookie})
	c3.expect("login_success")
	c4 := ts.connect("fourth", false)
	c4.send(map[string]any{"msg": "token_login", "cookie": cookie})
	c4.expect("login_fail")

	c2.send(map[string]any{"msg": "change_password", "cur_password": "wrong", "new_password": "next"})
	c2.expect("change_password_fail")
	c2.send(map[string]any{"msg": "change_password", "cur_password": "secret pw!%&", "new_password": "next"})
	c2.expect("change_password_done")
	c4.send(map[string]any{"msg": "login", "username": "Alice", "password": "next"})
	c4.expect("login_success")

	// An anonymous connection cannot touch rc files or start a game.
	anon := ts.connect("anon", false)
	anon.expect("lobby_complete")
	anon.send(map[string]any{"msg": "set_rc", "game_id": "test", "contents": "hacked"})
	anon.send(map[string]any{"msg": "play", "game_id": "test"})
	anon.expect("login_required")
	if rc, _ := os.ReadFile(filepath.Join(ts.dir, "rcs", ".rc")); len(rc) > 0 {
		t.Errorf("anonymous set_rc wrote a file")
	}
}

func TestPlayAndWatch(t *testing.T) {
	ts := startTestServer(t, nil)
	running := filepath.Join(ts.dir, "rcs", "running")

	// The player uses the compressed protocol, like a browser.
	player := ts.connect("player", true)
	player.expect("lobby_complete")
	player.register("Alice", "pw")
	player.send(map[string]any{"msg": "play", "game_id": "test"})
	client := player.expect("game_client")
	if !strings.Contains(fmt.Sprint(client["content"]), "/gamedata/"+fmt.Sprint(client["version"])+"/") {
		t.Errorf("game_client content does not reference its version")
	}
	player.expect("game_started")
	// With species and background set in the rc, crawl goes straight to the
	// game and sends the map.
	player.expect("player")
	player.expect("map")

	resp, err := http.Get("http://" + ts.addr + "/gamedata/" + fmt.Sprint(client["version"]) + "/game.js")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET game.js: %v %v", err, resp)
	}
	resp.Body.Close()

	locks := dirEntries(t, running)
	if len(locks) != 1 || !strings.HasPrefix(locks[0], "Alice:") || !strings.HasSuffix(locks[0], ".ttyrec") {
		t.Fatalf("in-progress locks: %v", locks)
	}
	lock, _ := os.ReadFile(filepath.Join(running, locks[0]))
	var pid, lines, cols int
	if n, _ := fmt.Sscanf(string(lock), "%d\n%d\n%d\n", &pid, &lines, &cols); n != 3 || lines != 24 || cols != 80 {
		t.Errorf("lock file contents: %q", lock)
	}

	// The lobby shows the game to someone who is not logged in.
	watcher := ts.connect("watcher", false)
	entry := watcher.expect("lobby_entry")
	if entry["username"] != "Alice" || entry["game_id"] != "test" {
		t.Errorf("lobby entry: %v", entry)
	}
	watcher.send(map[string]any{"msg": "watch", "username": "alice"})
	watcher.expect("game_client")
	watcher.expect("watching_started")
	// A new spectator is sent the whole map.
	watcher.expectMatch("map", func(m map[string]any) bool { return m["clear"] == true })
	player.expectMatch("update_spectators", func(m map[string]any) bool { return fmt.Sprint(m["count"]) == "1" })

	// The player moves; both see the result. 's' waits a turn.
	player.key('s')
	turn := func(m map[string]any) bool { return m["turn"] != nil }
	player.expectMatch("player", turn)
	watcher.expectMatch("player", turn)

	// Chat needs a login; the player's chat reaches the spectator.
	watcher.send(map[string]any{"msg": "chat_msg", "text": "hi"})
	if m := watcher.expect("chat"); !strings.Contains(fmt.Sprint(m["content"]), "need to log in") {
		t.Errorf("anonymous chat: %v", m)
	}
	player.send(map[string]any{"msg": "chat_msg", "text": "hello <b>there</b>"})
	chat := watcher.expectMatch("chat", func(m map[string]any) bool {
		return strings.Contains(fmt.Sprint(m["content"]), "chat_sender")
	})
	if content := fmt.Sprint(chat["content"]); !strings.Contains(content, "Alice") ||
		!strings.Contains(content, "hello &lt;b&gt;there&lt;/b&gt;") {
		t.Errorf("chat content: %s", content)
	}

	// Blocking anonymous spectators sends the watcher back to the lobby.
	player.send(map[string]any{"msg": "chat_msg", "text": "/block [anon]"})
	if m := watcher.expect("auth_error"); !strings.Contains(fmt.Sprint(m["reason"]), "restricted") {
		t.Errorf("blocked watcher: %v", m)
	}
	watcher.expect("go_lobby")
	watcher.send(map[string]any{"msg": "watch", "username": "Alice"})
	watcher.expect("auth_error")
	player.send(map[string]any{"msg": "chat_msg", "text": "/unblock [anon]"})
	player.expectMatch("chat", func(m map[string]any) bool {
		return strings.Contains(fmt.Sprint(m["content"]), "You have unblocked")
	})
	watcher.send(map[string]any{"msg": "watch", "username": "Alice"})
	watcher.expect("watching_started")

	// Leaving for the lobby saves the game (crawl calls a save on SIGHUP a
	// "disconnect"). The spectator is told and sent home.
	saved := func(m map[string]any) bool {
		return m["reason"] == "disconnect" && strings.Contains(fmt.Sprint(m["message"]), "Game saved")
	}
	player.send(map[string]any{"msg": "go_lobby"})
	if m := watcher.expect("game_ended"); !saved(m) {
		t.Errorf("spectator game_ended: %v", m)
	}
	watcher.expect("go_lobby")
	if m := player.expect("game_ended"); !saved(m) {
		t.Errorf("player game_ended: %v", m)
	}
	player.expect("lobby_complete")
	links := player.expectMatch("set_game_links", func(m map[string]any) bool {
		return strings.Contains(fmt.Sprint(m["content"]), "[")
	})
	if content := fmt.Sprint(links["content"]); !strings.Contains(content, "Alice") {
		t.Errorf("save info in game links: %s", content)
	}
	waitFor(t, "lock removal", func() bool { return len(dirEntries(t, running)) == 0 })

	ttyrecs := dirEntries(t, filepath.Join(ts.dir, "rcs", "ttyrecs", "Alice"))
	if len(ttyrecs) != 1 {
		t.Fatalf("ttyrecs: %v", ttyrecs)
	}
	rec, _ := os.ReadFile(filepath.Join(ts.dir, "rcs", "ttyrecs", "Alice", ttyrecs[0]))
	if !bytes.Contains(rec, []byte("Player: Alice")) {
		t.Errorf("ttyrec lacks its id header (%d bytes)", len(rec))
	}
}

// A game still running when its player disconnects, or when the server shuts
// down, is told to save and exit.
func TestDisconnectAndShutdown(t *testing.T) {
	ts := startTestServer(t, nil)
	running := filepath.Join(ts.dir, "rcs", "running")
	start := func(c *client, name string) {
		c.expect("lobby_complete")
		c.register(name, "pw")
		c.send(map[string]any{"msg": "play", "game_id": "test"})
		c.expect("game_started")
		c.expect("map")
	}

	alice := ts.connect("alice", false)
	start(alice, "Alice")
	alice.ws.Close()
	waitFor(t, "Alice's game to exit", func() bool { return len(dirEntries(t, running)) == 0 })

	bob := ts.connect("bob", false)
	start(bob, "Bob")
	go ts.stop()
	if m := bob.expect("close"); !strings.Contains(fmt.Sprint(m["reason"]), "Your game has been saved") {
		t.Errorf("shutdown message: %v", m)
	}
	bob.expectClosed()
	ts.stop()
	waitFor(t, "Bob's game to exit", func() bool { return len(dirEntries(t, running)) == 0 })
	var saves []string
	filepath.WalkDir(ts.dir, func(path string, _ os.DirEntry, _ error) error {
		if strings.HasSuffix(path, ".cs") {
			saves = append(saves, filepath.Base(path))
		}
		return nil
	})
	if len(saves) != 2 {
		t.Errorf("saved games: %v", saves)
	}
}

// A lock file left behind by a dead server is cleared; one naming a live
// process gets that process stopped before the new game starts.
func TestStaleLocks(t *testing.T) {
	ts := startTestServer(t, nil)
	running := filepath.Join(ts.dir, "rcs", "running")

	c := ts.connect("alice", false)
	c.expect("lobby_complete")
	c.register("Alice", "pw")

	// Garbage, and a pid that does not exist.
	os.WriteFile(filepath.Join(running, "Alice:2020-01-01.00:00:00.ttyrec"), []byte("garbage\n"), 0o644)
	os.WriteFile(filepath.Join(running, "Alice:2020-01-02.00:00:00.ttyrec"), []byte("999999999\n24\n80\n"), 0o644)
	c.send(map[string]any{"msg": "play", "game_id": "test"})
	c.expect("game_started")
	c.expect("map")
	if locks := dirEntries(t, running); len(locks) != 1 || strings.Contains(locks[0], "2020") {
		t.Errorf("locks after start: %v", locks)
	}
}

// A lock naming a live process that ignores SIGHUP: the player is warned, then
// asked whether to kill it, and the game starts once it is gone. Takes about
// twenty seconds, which is how long the server waits.
func TestStaleProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the stale-process timeouts")
	}
	ts := startTestServer(t, nil)
	running := filepath.Join(ts.dir, "rcs", "running")

	stale := exec.Command("sh", "-c", `trap "" HUP; while :; do sleep 0.2; done`)
	if err := stale.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { stale.Wait(); close(exited) }() // reap it, or it lingers as a zombie
	t.Cleanup(func() { stale.Process.Kill() })

	c := ts.connect("alice", false)
	c.expect("lobby_complete")
	c.register("Alice", "pw")
	lock := filepath.Join(running, "Alice:2020-01-01.00:00:00.ttyrec")
	os.WriteFile(lock, []byte(fmt.Sprintf("%d\n24\n80\n", stale.Process.Pid)), 0o644)

	c.send(map[string]any{"msg": "play", "game_id": "test"})
	if m := c.expect("stale_processes"); m["game"] != "Test game" {
		t.Errorf("stale_processes: %v", m)
	}
	c.expect("force_terminate?")
	select {
	case <-exited:
		t.Fatal("stale process died before the player agreed to kill it")
	default:
	}
	c.send(map[string]any{"msg": "force_terminate", "answer": true})
	c.expect("hide_dialog")
	c.expect("map")
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Error("stale process was not killed")
	}
	if _, err := os.Stat(lock); err == nil {
		t.Error("stale lock file still exists")
	}
}

// startGame logs a new user in on c and starts their game.
func startGame(c *client, name string) {
	c.t.Helper()
	c.expect("lobby_complete")
	c.register(name, "pw")
	c.send(map[string]any{"msg": "play", "game_id": "test"})
	c.expect("game_started")
	c.expect("map")
}

var gameMessages = map[string]bool{"map": true, "player": true, "msgs": true, "game_client": true,
	"update_spectators": true, "chat": true, "ui-state": true, "menu": true}

// Spectators hop between two busy games. Whatever a game sends after a
// spectator has left it must not reach them: between returning to the lobby
// and joining the next game they may see no game traffic, and each time they
// join they are sent the full map.
func TestSpectatorsHopping(t *testing.T) {
	ts := startTestServer(t, nil)
	players := []*client{ts.connect("alice", false), ts.connect("bob", true)}
	names := []string{"Alice", "Bob"}
	for i, p := range players {
		startGame(p, names[i])
	}

	// Keep both games producing output.
	stop := make(chan struct{})
	var busy sync.WaitGroup
	for _, p := range players {
		busy.Add(1)
		go func() {
			defer busy.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(15 * time.Millisecond):
					p.key('s')
				}
			}
		}()
	}
	// Players' incoming messages are not interesting here; keep them drained.
	for _, p := range players {
		go func() {
			for range p.msgs {
			}
		}()
	}

	var hoppers sync.WaitGroup
	for h := 0; h < 4; h++ {
		hoppers.Add(1)
		go func() {
			defer hoppers.Done()
			c := ts.connect(fmt.Sprintf("hopper%d", h), h%2 == 0)
			c.expect("lobby_complete")
			for round := 0; round < 12; round++ {
				target := names[(h+round)%2]
				c.send(map[string]any{"msg": "watch", "username": target})
				if m := c.expect("watching_started"); m["username"] != target {
					t.Errorf("hopper %d: watching %v, asked for %s", h, m["username"], target)
				}
				c.expectMatch("map", func(m map[string]any) bool { return m["clear"] == true })
				if round%3 == 0 {
					c.expectMatch("player", func(m map[string]any) bool { return m["turn"] != nil })
				}
				if round%2 == 0 {
					// Via the lobby; otherwise straight to the other game.
					c.send(map[string]any{"msg": "go_lobby"})
					c.expect("go_lobby")
					c.expect("lobby_complete")
					quiet := time.After(60 * time.Millisecond)
				lobby:
					for {
						select {
						case m := <-c.msgs:
							if gameMessages[fmt.Sprint(m["msg"])] {
								t.Errorf("hopper %d: got '%v' from a game after returning to the lobby", h, m["msg"])
							}
						case <-quiet:
							break lobby
						}
					}
				}
			}
		}()
	}
	hoppers.Wait()
	close(stop)
	busy.Wait()
}

// A game that hits an internal error ends by itself: its crawl process is
// stopped, its player lands in the lobby, and the game next to it carries on.
func TestGameFailureIsContained(t *testing.T) {
	ts := startTestServer(t, nil)
	running := filepath.Join(ts.dir, "rcs", "running")
	alice, bob := ts.connect("alice", false), ts.connect("bob", false)
	startGame(alice, "Alice")
	startGame(bob, "Bob")

	g := ts.srv.hub.findGame("Alice")
	if g == nil {
		t.Fatal("Alice's game is not registered")
	}
	g.do(func() { panic("injected failure") })

	if m := alice.expect("game_ended"); m["reason"] != "error" {
		t.Errorf("game_ended after failure: %v", m)
	}
	alice.expect("lobby_complete")
	waitFor(t, "Alice's crawl to be stopped", func() bool {
		locks := dirEntries(t, running)
		return len(locks) == 1 && strings.HasPrefix(locks[0], "Bob:")
	})

	bob.key('s')
	bob.expectMatch("player", func(m map[string]any) bool { return m["turn"] != nil })
	// And Alice can start again.
	alice.send(map[string]any{"msg": "play", "game_id": "test"})
	alice.expect("game_started")
	alice.expect("map")
}

// A game whose goroutine is stuck does not hold up the lobby or other games.
func TestStuckGameDelaysNobody(t *testing.T) {
	ts := startTestServer(t, nil)
	alice, bob := ts.connect("alice", false), ts.connect("bob", false)
	startGame(alice, "Alice")
	startGame(bob, "Bob")

	release := make(chan struct{})
	ts.srv.hub.findGame("Alice").do(func() { <-release })
	defer close(release)

	start := time.Now()
	bob.key('s')
	bob.expectMatch("player", func(m map[string]any) bool { return m["turn"] != nil })
	visitor := ts.connect("visitor", false)
	visitor.expect("lobby_complete")
	visitor.send(map[string]any{"msg": "watch", "username": "Bob"})
	visitor.expect("watching_started")
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("other activity took %v while a game was stuck", took)
	}
}

// The game list can change while the server runs, as it does when a new crawl
// build is installed: new games appear, and a game in progress is untouched.
func TestReloadGames(t *testing.T) {
	ts := startTestServer(t, nil)
	source := crawlSource(t)
	alice := ts.connect("alice", false)
	startGame(alice, "Alice")

	writeGameDef(t, ts.dir, source, "newer", "Newer game")
	if err := ts.srv.ReloadGames(); err != nil {
		t.Fatal(err)
	}
	// A broken definition is rejected and changes nothing.
	os.WriteFile(filepath.Join(ts.dir, "games.d", "broken.yaml"),
		[]byte("games:\n  - id: broken\n    name: Broken\n"), 0o644)
	if err := ts.srv.ReloadGames(); err == nil {
		t.Error("a game definition with no crawl_binary was accepted")
	}
	os.Remove(filepath.Join(ts.dir, "games.d", "broken.yaml"))

	alice.key('s')
	alice.expectMatch("player", func(m map[string]any) bool { return m["turn"] != nil })

	bob := ts.connect("bob", false)
	bob.expect("lobby_complete")
	bob.register("Bob", "pw")
	links := fmt.Sprint(bob.expect("set_game_links")["content"])
	if !strings.Contains(links, `#play-test"`) || !strings.Contains(links, `<a href="#play-newer">Newer game</a>`) {
		t.Errorf("game links after reload: %s", links)
	}
	bob.send(map[string]any{"msg": "play", "game_id": "newer"})
	bob.expect("game_started")
	bob.expect("map")
}

// freePort returns a TCP port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// The private listener: metrics, and the admin pages with their actions.
func TestAdminAndMetrics(t *testing.T) {
	adminAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	ts := startTestServer(t, func(cfg *config.Config) {
		cfg.AdminAddress = adminAddr
		cfg.NewAccountsDisabled = true
	})
	base := "http://" + adminAddr
	get := func(path string) string {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s = %d: %s", path, resp.StatusCode, body)
		}
		return string(body)
	}
	page := get("/admin/")
	m := regexp.MustCompile(`name="token" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("admin page has no form token")
	}
	token := m[1]
	// post submits an admin form and returns the page it lands on.
	post := func(action string, fields url.Values, withToken bool) (int, string) {
		t.Helper()
		if withToken {
			fields.Set("token", token)
		}
		resp, err := http.PostForm(base+"/admin/"+action, fields)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, html.UnescapeString(string(body))
	}
	do := func(action string, fields url.Values) string {
		t.Helper()
		status, body := post(action, fields, true)
		if status != 200 || !strings.Contains(body, "Done: ") {
			t.Fatalf("admin %s %v: status %d, page says: %s", action, fields, status,
				regexp.MustCompile(`(?s)<div class="note.*?</div>`).FindString(body))
		}
		return body
	}

	// The public listener serves neither.
	for _, path := range []string{"/admin/", "/metrics"} {
		if resp, err := http.Get("http://" + ts.addr + path); err != nil || resp.StatusCode != 404 {
			t.Errorf("public GET %s: %v %v", path, err, resp.StatusCode)
		}
	}
	// A page on another site cannot drive the admin pages: its form has no
	// token, and a rebound DNS name is not an accepted host.
	if status, _ := post("announce", url.Values{"text": {"x"}}, false); status != http.StatusForbidden {
		t.Errorf("action without token: status %d", status)
	}
	req, _ := http.NewRequest("GET", base+"/admin/", nil)
	req.Host = "evil.example"
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("admin page under a foreign host name: %v %v", err, resp.StatusCode)
	}

	// Accounts: create one although registration is closed, and use it.
	do("create-user", url.Values{"username": {"Alice"}, "password": {"pw"}})
	do("create-user", url.Values{"username": {"Bob"}, "password": {"pw"}})
	if status, body := post("create-user", url.Values{"username": {"Alice"}, "password": {"pw"}}, true); status != 200 ||
		!strings.Contains(body, "Failed: User already exists!") {
		t.Errorf("duplicate user: %d", status)
	}
	alice := ts.connect("alice", false)
	alice.expect("lobby_complete")
	login := func(c *client, name, password string) {
		c.send(map[string]any{"msg": "login", "username": name, "password": password})
	}
	login(alice, "Alice", "pw")
	alice.expect("login_success")
	alice.send(map[string]any{"msg": "play", "game_id": "test"})
	alice.expect("game_started")
	alice.expect("map")

	bob := ts.connect("bob", false)
	bob.expect("lobby_complete")
	login(bob, "Bob", "pw")
	bob.expect("login_success")
	bob.send(map[string]any{"msg": "watch", "username": "Alice"})
	bob.expect("watching_started")

	page = get("/admin/")
	for _, want := range []string{"Games in progress (1)", "Connections (2)", "playing test", "watching Alice",
		"Accounts (2)", "Game definitions (1)"} {
		if !strings.Contains(page, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}

	// An announcement reaches both.
	do("announce", url.Values{"text": {"Server restarts at noon"}})
	for _, c := range []*client{alice, bob} {
		if m := c.expect("server_announcement"); m["text"] != "Server restarts at noon" {
			t.Errorf("%s: announcement %v", c.name, m)
		}
	}

	// A new password takes effect; the old one stops working.
	do("set-password", url.Values{"username": {"Bob"}, "password": {"better"}})
	probe := ts.connect("probe", false)
	probe.expect("lobby_complete")
	login(probe, "Bob", "pw")
	probe.expect("login_fail")
	login(probe, "Bob", "better")
	probe.expect("login_success")

	// Banning Alice logs her out at once and saves her game.
	do("set-status", url.Values{"username": {"Alice"}, "status": {"ban"}, "on": {"1"}})
	if m := alice.expect("logout"); !strings.Contains(fmt.Sprint(m["reason"]), "disabled") {
		t.Errorf("banned player: %v", m)
	}
	bob.expect("game_ended")
	waitFor(t, "Alice's game to stop", func() bool { return len(dirEntries(t, filepath.Join(ts.dir, "rcs", "running"))) == 0 })
	login(probe, "Alice", "pw")
	if m := probe.expect("login_fail"); !strings.Contains(fmt.Sprint(m["reason"]), "disabled") {
		t.Errorf("banned login: %v", m)
	}
	do("set-status", url.Values{"username": {"Alice"}, "status": {"ban"}, "on": {"0"}})
	do("set-status", url.Values{"username": {"Bob"}, "status": {"admin"}, "on": {"1"}})

	// Stopping a game and disconnecting a session.
	carol := ts.connect("carol", false)
	carol.expect("lobby_complete")
	login(carol, "Alice", "pw")
	carol.expect("login_success")
	carol.send(map[string]any{"msg": "play", "game_id": "test"})
	carol.expect("game_started")
	carol.expect("map")
	page = get("/admin/")
	gameID := regexp.MustCompile(`action="/admin/stop-game">.*?\s*<input type="hidden" name="id" value="(\d+)"`).FindStringSubmatch(page)
	if gameID == nil {
		t.Fatal("no stop button on the admin page")
	}
	do("stop-game", url.Values{"id": {gameID[1]}})
	carol.expect("game_ended")
	carol.expect("lobby_complete")
	sessID := regexp.MustCompile(`action="/admin/disconnect">.*?\s*<input type="hidden" name="id" value="(\d+)"`).FindStringSubmatch(page)
	do("disconnect", url.Values{"id": {sessID[1]}})
	do("reload-games", url.Values{})

	metrics := get("/metrics")
	for _, want := range []string{
		`geryon_games_started_total{game_id="test"} 2`,
		`geryon_games_ended_total{game_id="test",reason="disconnect"} 2`,
		`geryon_logins_total{method="password",result="success"} 4`,
		`geryon_logins_total{method="password",result="failure"} 2`,
		`geryon_game_list_reloads_total{result="success"} 1`,
		`geryon_connections{state="lobby"}`,
		`geryon_games{game_id="test"} 0`,
		`geryon_websocket_sent_bytes_total{stage="wire"}`,
		`geryon_crawl_messages_total`,
		`geryon_build_info{version="dev"} 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestOutbox(t *testing.T) {
	aborted := false
	o := newOutbox(func() { aborted = true })
	lobby := o.Advance()
	if !o.SendAt(lobby, "a") || !o.Send("b") {
		t.Fatal("sends in the current epoch were refused")
	}
	game := o.Advance()
	if o.SendAt(lobby, "stale") {
		t.Error("a send from a place the connection has left was accepted")
	}
	if !o.SendAt(game, "c") {
		t.Error("a send in the new epoch was refused")
	}
	for _, want := range []string{"a", "b", "c"} {
		if got := <-o.frames; got != want {
			t.Errorf("frame %q, want %q", got, want)
		}
	}
	// A reader that never drains is cut off rather than blocking senders.
	for i := 0; i < cap(o.frames); i++ {
		o.Send("x")
	}
	if aborted || o.Send("one too many") || !aborted {
		t.Errorf("overflow: aborted=%v", aborted)
	}
	if o.Send("after") {
		t.Error("send accepted after the outbox closed")
	}
	o.Close() // harmless when already closed
}

func TestParseWhereData(t *testing.T) {
	got := parseWhereData("name=Alice:xl=3:place=D::2:title=the Chopper:status=active\n")
	want := map[string]any{"name": "Alice", "xl": "3", "place": "D:2", "title": "the Chopper", "status": "active"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseWhereData = %v, want %v", got, want)
	}
}
