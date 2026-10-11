package server

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/smw/geryon/internal/config"
	"github.com/smw/geryon/internal/tmpl"
)

// Game is one crawl game: the crawl process, the socket it talks over, and
// the roster of browsers playing and watching.
//
// A game runs on its own goroutine (run), which owns every field below the
// marked line. Sessions reach it only through do, join and Stop, and the game
// never waits on a session: it writes to outboxes, which don't block, and
// posts to sessions, which doesn't either. So a slow or stuck game or browser
// holds up nobody else.
type Game struct {
	srv      *Server
	id       int
	def      *config.Game
	username string
	dglLess  bool // the single anonymous game of a server without accounts

	playerSeat int // seat id of the session that started the game

	inbox    chan func()
	stopCh   chan struct{} // closed to ask the game to save and exit
	stopOnce sync.Once
	done     chan struct{} // closed when run has returned

	lastActivity atomic.Int64 // unix nanoseconds
	pid          atomic.Int64 // the crawl process, once started
	created      time.Time    // never changes
	log          *gameLog     // has its own lock

	// ---- owned by the game's goroutine ----

	seats    map[int]*seat
	lastSeat int
	lastJoin time.Time

	term *terminal
	conn *crawlConn

	pending       []pendingMsg // crawl messages not yet sent on
	queueMessages bool         // crawl marks the end of each batch with flush_messages
	useStdout     bool         // relay terminal output until the socket speaks

	clientPath   string
	crawlVersion string

	where            map[string]any
	lastMilestone    map[string]any
	directMilestones bool
	nextWhereCheck   time.Time
	whereTime        time.Time
	wasIdle          bool

	formattedTime  string
	lockBasename   string
	socketPath     string
	ttyrecFilename string
	lockPath       string
	lockFile       *os.File

	exitReason  string
	exitMessage string
	exitDumpURL string
	lastStderr  string // crawl's most recent line on stderr

	launched bool // crawl was started and the game registered

	// Stopping: SIGHUP, then SIGABRT, then SIGKILL, kill_timeout apart.
	stopping  bool
	killStage int
	killC     <-chan time.Time

	// Startup, while an earlier process is being cleared out.
	cancelled   bool
	forceAnswer *bool

	blocked map[string]bool
	kicked  map[string]kick // not kept across sessions
}

// seatInfo is what a game knows about a browser in its roster.
type seatInfo struct {
	sess       *Session
	out        *Outbox
	epoch      uint64
	name       string // "" for an anonymous spectator
	admin      bool
	restricted bool // anonymous, or an account on hold
}

type seat struct {
	seatInfo
	id         int
	player     bool
	fresh      bool // a spectator still owed the full map
	chatHidden bool
}

func (st *seat) send(msgs ...string) { st.out.SendAt(st.epoch, frame(msgs...)) }

type kick struct {
	start    time.Time
	interval time.Duration
}

// pendingMsg is a message from crawl waiting to go out. only, if set, limits
// it to those seats.
type pendingMsg struct {
	text string
	only map[int]bool
}

// newGame creates a game with player in the first seat. The caller starts it
// with `go g.run()`.
func newGame(srv *Server, def *config.Game, username string, dglLess bool, player seatInfo) *Game {
	now := time.Now().UTC()
	g := &Game{
		srv:           srv,
		id:            srv.hub.newGameID(),
		def:           def,
		username:      username,
		dglLess:       dglLess,
		playerSeat:    1,
		inbox:         make(chan func(), 256),
		stopCh:        make(chan struct{}),
		done:          make(chan struct{}),
		seats:         map[int]*seat{},
		lastSeat:      1,
		useStdout:     true,
		where:         map[string]any{},
		formattedTime: now.Format("2006-01-02.15:04:05"),
		created:       now,
		log:           &gameLog{},
		blocked:       map[string]bool{},
		kicked:        map[string]kick{},
	}
	g.seats[1] = &seat{seatInfo: player, id: 1, player: true}
	g.lockBasename = g.formattedTime + ".ttyrec"
	g.clientPath = g.configPath("client_path")
	g.lastActivity.Store(time.Now().UnixNano())
	return g
}

// logf logs to the server log and to the game's own log.
func (g *Game) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("P%-5d %s", g.id, msg)
	g.log.add("server", msg)
}

func (g *Game) configPath(key string) string { return g.def.Str(key, g.username) }

// ---- the interface for sessions ----

// do runs f on the game's goroutine. It reports false if the game has ended.
func (g *Game) do(f func()) bool {
	select {
	case g.inbox <- f:
		return true
	case <-g.done:
		return false
	}
}

// Stop asks the game to save and exit. It returns at once; the game tells
// its seats when it has ended.
func (g *Game) Stop() { g.stopOnce.Do(func() { close(g.stopCh) }) }

// IdleSeconds is how long it has been since the player did anything.
func (g *Game) IdleSeconds() int {
	return int(time.Since(time.Unix(0, g.lastActivity.Load())).Seconds())
}

type joinResult struct {
	seatID  int
	ok      bool
	refusal string
}

// join seats a spectator. If refused, refusal may hold a reason to show.
func (g *Game) join(info seatInfo) (seatID int, ok bool, refusal string) {
	reply := make(chan joinResult, 1)
	if !g.do(func() { reply <- g.addSpectator(info) }) {
		return 0, false, ""
	}
	select {
	case r := <-reply:
		return r.seatID, r.ok, r.refusal
	case <-g.done:
		return 0, false, ""
	}
}

// ---- life cycle ----

// run is the game's life: clear out any earlier process, start crawl, relay
// until crawl exits, tidy up.
func (g *Game) run() {
	defer close(g.done)
	defer g.finish()
	defer func() {
		if r := recover(); r != nil {
			g.logf("Internal error, ending game: %v\n%s", r, debug.Stack())
			g.exitReason, g.exitMessage = "error", "The server hit an internal error; your game has been saved."
			g.srv.metrics.failures.WithLabelValues("game").Inc()
			g.emergencyStop()
		}
	}()

	player := g.seats[g.playerSeat]
	g.sendClient(player)
	g.describeSpectators()

	if !g.clearStaleLocks() || !g.launch() {
		return
	}
	g.srv.hub.addGame(g)
	g.launched = true
	g.srv.metrics.gamesStarted.WithLabelValues(g.def.ID).Inc()
	player.send(encodeMessage("game_started", nil))
	g.restoreBlocklist()
	g.notify(player, "'/help' to see available chat commands")
	if g.srv.cfg.MaxChatLength == 0 {
		g.notify(player, "Regular chat is disabled on this server")
	}
	g.publish()
	g.loop()
}

func (g *Game) fail(message string) {
	g.exitReason, g.exitMessage, g.exitDumpURL = "error", message, ""
}

const staleProcessError = "Error while trying to terminate a stale process.\nPlease contact an administrator."
const startError = "Error while starting the Crawl process!\nSomething has gone very wrong; please let a server admin know."

// wait handles the inbox for up to d, or until done reports true. It returns
// false if the start was cancelled meanwhile.
func (g *Game) wait(d time.Duration, done func() bool) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	poll := time.NewTicker(200 * time.Millisecond) // done may depend on the outside world
	defer poll.Stop()
	for {
		if g.cancelled {
			return false
		}
		if done != nil && done() {
			return true
		}
		select {
		case f := <-g.inbox:
			f()
		case <-g.stopCh:
			g.cancelled = true
		case <-poll.C:
		case <-timer.C:
			return true
		}
	}
}

// findLock returns the lock file of a game this player already has in
// progress, if any.
func (g *Game) findLock() (string, error) {
	dir := g.configPath("inprogress_path")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), g.username+":") && strings.HasSuffix(e.Name(), ".ttyrec") {
			return filepath.Join(dir, e.Name()), nil
		}
	}
	return "", nil
}

// processExists reports whether pid is a live process we may signal. One we
// may not signal is not a game of ours, whatever a lock file says.
func processExists(pid int) bool { return syscall.Kill(pid, 0) == nil }

// clearStaleLocks makes sure no earlier process holds the player's save. A
// lock file names such a process - a game in another tab, or one a dead
// server left behind. The player is warned, the process is asked to exit
// (SIGHUP makes crawl save), and if it is still there ten seconds later the
// player is asked whether to kill it. Returns false if the game should not
// start.
func (g *Game) clearStaleLocks() bool {
	player := g.seats[g.playerSeat]
	warned, dialog := false, false
	for {
		lockfile, err := g.findLock()
		if err != nil {
			g.logf("Cannot read the in-progress directory: %v", err)
			g.fail(startError)
			return false
		}
		if lockfile == "" {
			if dialog {
				player.send(encodeMessage("hide_dialog", nil))
			}
			return true
		}
		data, err := os.ReadFile(lockfile)
		if err != nil {
			g.logf("Error while handling lockfile %s: %v", lockfile, err)
			g.fail(staleProcessError)
			return false
		}
		line, _, _ := strings.Cut(string(data), "\n")
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 0 {
			// Empty or corrupt, as after a server crash.
			g.logf("Invalid PID from lockfile `%s`, clearing", lockfile)
			os.Remove(lockfile)
			continue
		}
		if !processExists(pid) {
			g.logf("Clearing stale lock at %s, pid %d is gone.", lockfile, pid)
			os.Remove(lockfile)
			continue
		}

		gone := func() bool { return !processExists(pid) }
		if !warned {
			// Give the player a moment to back out.
			const hupWait = 10
			warned, dialog = true, true
			player.send(encodeMessage("stale_processes", M{"timeout": hupWait, "game": g.def.Str("name", g.username)}))
			if !g.wait(hupWait*time.Second, nil) {
				return false
			}
		}
		g.logf("Purging stale lock at %s, pid %d.", lockfile, pid)
		syscall.Kill(pid, syscall.SIGHUP)
		if !g.wait(10*time.Second, gone) {
			return false
		}
		if !gone() {
			g.logf("Couldn't terminate pid %d gracefully.", pid)
			dialog = true
			g.forceAnswer = nil
			player.send(encodeMessage("force_terminate?", nil))
			if !g.wait(time.Hour, func() bool { return g.forceAnswer != nil || gone() }) {
				return false
			}
			if !gone() {
				if g.forceAnswer == nil || !*g.forceAnswer {
					return false
				}
				g.logf("Terminating pid %d forcefully!", pid)
				syscall.Kill(pid, syscall.SIGABRT)
				if !g.wait(5*time.Second, gone) {
					return false
				}
			}
		}
		os.Remove(lockfile)
	}
}

func (g *Game) baseCall() []string {
	if g.dglLess {
		return []string{"./crawl"}
	}
	call := append(g.def.CallBase(),
		"-name", g.username,
		"-rc", filepath.Join(g.configPath("rcfile_path"), g.username+".rc"),
		"-macro", filepath.Join(g.configPath("macro_path"), g.username+".macro"),
		"-morgue", g.configPath("morgue_path"))
	if g.seats[g.playerSeat].restricted {
		call = append(call, "-no-player-bones")
	}
	call = append(call, g.def.List("options", g.username)...)
	if g.def.Has("dir_path") {
		call = append(call, "-dir", g.configPath("dir_path"))
	}
	return call
}

func (g *Game) ttyrecIDHeader() []byte {
	const clrscr, crlf = "\033[2J", "\r\n"
	now := time.Now()
	return []byte(clrscr + "\033[1;1H" + crlf +
		"Player: " + g.username + crlf +
		"Game: " + g.def.Str("name", g.username) + crlf +
		"Server: " + g.srv.cfg.ServerID + crlf +
		"Filename: " + g.lockBasename + crlf +
		fmt.Sprintf("Time: (%d) %s", now.Unix(), now.Format(time.ANSIC)) + crlf +
		clrscr)
}

// launch starts the crawl process and writes its lock file. On failure it
// has recorded why and left nothing running.
func (g *Game) launch() bool {
	cfg := g.srv.cfg
	g.socketPath = filepath.Join(g.configPath("socket_path"), g.username+":"+g.formattedTime+".sock")
	// A Unix socket address holds 107 bytes; crawl does not check, and aborts.
	if len(g.socketPath) > 107 {
		g.logf("Game socket path is too long (%d bytes, limit 107): %s", len(g.socketPath), g.socketPath)
		g.fail(startError)
		return false
	}
	if err := os.Remove(g.socketPath); err != nil && !os.IsNotExist(err) {
		g.logf("Cannot remove old socket %s: %v", g.socketPath, err)
		g.fail(startError)
		return false
	}
	call := append(g.baseCall(), "-webtiles-socket", g.socketPath, "-await-connection")
	if dir := g.configPath("ttyrec_path"); dir != "" && cfg.EnableTtyrecs {
		g.ttyrecFilename = filepath.Join(dir, g.lockBasename)
	}
	if cfg.DGLMode {
		g.logf("Starting %s for %s.", g.def.ID, g.username)
	} else {
		g.logf("Starting game.")
	}

	term, err := startTerminal(call, g.def.Map("env", g.username), g.def.Str("cwd", g.username),
		cfg.RecordingTermSize, g.ttyrecFilename, g.ttyrecIDHeader(), g.done)
	if err != nil {
		g.logf("Error while starting the Crawl process: %v", err)
		g.fail(startError)
		return false
	}
	g.term = term
	g.pid.Store(int64(term.pid))
	g.lastActivity.Store(time.Now().UnixNano())

	if err := g.writeLock(); err != nil {
		g.logf("Error while starting the Crawl process: %v", err)
		g.fail(startError)
		g.emergencyStop()
		return false
	}
	return true
}

// writeLock writes the lock file that marks the player's game as running:
// the crawl pid and the terminal size, as dgamelaunch does it.
func (g *Game) writeLock() error {
	g.lockPath = filepath.Join(g.configPath("inprogress_path"), g.username+":"+g.lockBasename)
	f, err := os.Create(g.lockPath)
	if err != nil {
		return err
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
		f.Close()
		os.Remove(g.lockPath)
		return err
	}
	size := g.srv.cfg.RecordingTermSize
	fmt.Fprintf(f, "%d\n%d\n%d\n", g.term.pid, size[1], size[0])
	g.lockFile = f
	return nil
}

// loop relays between crawl and the roster until the crawl process exits.
func (g *Game) loop() {
	term := g.term
	idle := time.NewTicker(10 * time.Second)
	defer idle.Stop()
	// Crawl creates its socket shortly after starting, then waits for us.
	connect := time.NewTicker(20 * time.Millisecond)
	defer connect.Stop()
	connectC := connect.C

	stopC := g.stopCh
	output, stderr := term.output, term.stderr
	var crawlMsgs <-chan []string
	var crawlGone <-chan struct{}

	for {
		select {
		case f := <-g.inbox:
			f()
		case <-stopC:
			stopC = nil
			g.beginStop()
		case <-g.killC:
			g.escalateStop()

		case <-connectC:
			if _, err := os.Stat(g.socketPath); err != nil {
				continue
			}
			connect.Stop()
			connectC = nil
			local := filepath.Join(g.srv.sockDir, fmt.Sprintf("crawl%d.socket", g.id))
			conn, err := attachCrawl(g.socketPath, local, true, g.done)
			if err != nil {
				g.logf("Could not open a game socket: %v", err)
				g.fail(startError)
				g.beginStop()
				continue
			}
			g.conn, crawlMsgs, crawlGone = conn, conn.msgs, conn.gone
		case msgs := <-crawlMsgs:
			for _, m := range msgs {
				g.onCrawlMessage(m)
			}
		case <-crawlGone:
			crawlGone = nil
			g.beginStop()

		case lines, open := <-output:
			if !open {
				output = nil
				continue
			}
			g.onTerminalOutput(lines)
		case line, open := <-stderr:
			if !open {
				stderr = nil
				continue
			}
			g.onCrawlStderr(line)

		case <-idle.C:
			g.checkIdle()
			g.checkWhere()

		case <-term.exited:
			// Pick up what crawl said on its way out: stderr is closed by now,
			// and its last socket messages (the exit reason) are at most a
			// moment behind.
			if stderr != nil {
				for line := range stderr {
					g.onCrawlStderr(line)
				}
			}
			for crawlMsgs != nil {
				select {
				case msgs := <-crawlMsgs:
					for _, m := range msgs {
						g.onCrawlMessage(m)
					}
				case <-time.After(30 * time.Millisecond):
					crawlMsgs = nil
				}
			}
			g.logf("Crawl PID %d terminated.", term.pid)
			g.term = nil
			if g.exitReason == "" && term.failed {
				// Crawl died without saying why. Tell the player something,
				// with its last words if it had any.
				g.exitReason, g.exitMessage = "error", "The game ended unexpectedly."
				if g.lastStderr != "" {
					g.exitMessage += "\n" + g.lastStderr
				}
			}
			return
		}
	}
}

// beginStop asks crawl to save and exit.
func (g *Game) beginStop() {
	if g.stopping || g.term == nil {
		return
	}
	g.stopping = true
	g.term.flush()
	if err := g.term.signal(syscall.SIGHUP); err != nil && !errors.Is(err, os.ErrProcessDone) {
		g.logf("Error %v on SIGHUP to child process", err)
	}
	g.killC = time.After(seconds(g.srv.cfg.KillTimeout))
}

// escalateStop deals with a crawl that did not exit when asked.
func (g *Game) escalateStop() {
	g.killStage++
	if g.killStage == 1 {
		g.logf("Killing crawl process after SIGHUP did nothing.")
		g.term.signal(syscall.SIGABRT)
		g.killC = time.After(seconds(g.srv.cfg.KillTimeout))
	} else {
		g.logf("Crawl process survived SIGABRT; sending SIGKILL.")
		g.term.signal(syscall.SIGKILL)
		g.killC = nil
	}
}

// emergencyStop ends the crawl process without the normal loop, for when the
// game cannot go on.
func (g *Game) emergencyStop() {
	term := g.term
	if term == nil {
		return
	}
	g.term = nil
	go func() { // keep the terminal's channels drained so the process can exit
		for range term.output {
		}
	}()
	go func() {
		for range term.stderr {
		}
	}()
	// Until crawl has set up its handlers it ignores SIGHUP, so don't wait long.
	term.signal(syscall.SIGHUP)
	select {
	case <-term.exited:
		return
	case <-time.After(2 * time.Second):
	}
	term.signal(syscall.SIGKILL)
	<-term.exited
}

// finish tidies up after the game and tells everyone it is over.
func (g *Game) finish() {
	if m := g.srv.metrics; g.launched {
		reason := g.exitReason
		if reason == "" {
			reason = "unknown"
		}
		m.gamesEnded.WithLabelValues(g.def.ID, reason).Inc()
		m.gameDuration.Observe(time.Since(g.created).Seconds())
	} else {
		m.startFailures.Inc()
	}
	if g.conn != nil {
		g.conn.close()
		g.conn = nil
	}
	if g.lockFile != nil {
		g.lockFile.Close() // closing releases the lock
		os.Remove(g.lockPath)
	}
	if g.socketPath != "" {
		os.Remove(g.socketPath)
	}
	g.flush()
	g.srv.hub.removeGame(g, g.exitReason, g.exitMessage, g.exitDumpURL)

	ended := encodeMessage("game_ended", M{"reason": orNull(g.exitReason),
		"message": orNull(g.exitMessage), "dump": orNull(g.exitDumpURL)})
	failed := g.exitReason == "error"
	for _, st := range g.seats {
		if !st.player || g.exitReason != "" {
			st.send(ended)
		}
		st.sess.post(func() { st.sess.seatClosed(g, "", failed) })
	}
}

// ---- the browser client for this game's crawl version ----

// sendClient sends a seat the game's HTML, and registers the client's scripts
// and tiles under /gamedata/<version>/. The version is a hash, so browsers
// can cache each crawl version's client separately.
func (g *Game) sendClient(st *seat) {
	if g.clientPath == "" {
		return // crawl will say where its client is
	}
	abs, _ := filepath.Abs(g.clientPath)
	sum := sha1.Sum([]byte(abs + g.crawlVersion))
	version := hex.EncodeToString(sum[:])

	// The client that belongs to this game's crawl build.
	files := os.DirFS(g.clientPath)
	static, _ := fs.Sub(files, "static")
	g.srv.registerClient(version, static)

	src, err := fs.ReadFile(files, "templates/game.html")
	var page string
	if err == nil {
		page, err = tmpl.Render(string(src), &tmpl.Env{Values: map[string]any{"version": version}})
	}
	if err != nil {
		g.logf("Could not render the game client from '%s': %v", g.clientPath, err)
		return
	}
	st.send(encodeMessage("game_client", M{"version": version, "content": page}))
}

// ---- messages from crawl ----

var (
	fullMapMsg = regexp.MustCompile(`"msg" *: *"map"`)
	clearTrue  = regexp.MustCompile(`"clear" *: *true`)
	spectOnly  = regexp.MustCompile(`"spect_only" *: *true`)
)

// relay queues a game message for the roster. A full map sent while new
// spectators are waiting was produced for them: it can run to hundreds of
// kilobytes, so if crawl marked it spectator-only it goes to the new
// spectators alone. Crawl puts the fields checked here first.
func (g *Game) relay(msg string) {
	item := pendingMsg{text: msg}
	head := msg[:min(len(msg), 50)]
	if fullMapMsg.MatchString(head) && clearTrue.MatchString(head) {
		fresh := map[int]bool{}
		for _, st := range g.seats {
			if st.fresh {
				fresh[st.id] = true
				st.fresh = false
			}
		}
		if len(fresh) > 0 && spectOnly.MatchString(head) {
			item.only = fresh
		}
	}
	g.pending = append(g.pending, item)
	if !g.queueMessages {
		g.flush()
	}
}

// flush sends the queued messages to the roster as one frame each, so a
// browser applies a whole turn's changes together.
func (g *Game) flush() {
	if len(g.pending) == 0 {
		return
	}
	targeted := false
	texts := make([]string, len(g.pending))
	for i, m := range g.pending {
		texts[i] = m.text
		targeted = targeted || m.only != nil
	}
	if !targeted {
		// The usual case: everyone gets the same frame, built once.
		shared := frame(texts...)
		for _, st := range g.seats {
			st.out.SendAt(st.epoch, shared)
		}
	} else {
		for _, st := range g.seats {
			var mine []string
			for _, m := range g.pending {
				if m.only == nil || m.only[st.id] {
					mine = append(mine, m.text)
				}
			}
			if len(mine) > 0 {
				st.send(mine...)
			}
		}
	}
	g.pending = g.pending[:0]
}

func (g *Game) broadcast(msg string) {
	for _, st := range g.seats {
		st.send(msg)
	}
}

func (g *Game) noteActivity() {
	g.lastActivity.Store(time.Now().UnixNano())
	g.checkIdle()
}

func (g *Game) onCrawlMessage(msg string) {
	// Terminal output is only relayed for the sake of wrapper scripts; once
	// crawl itself speaks on the socket that stops.
	g.useStdout = false
	g.srv.metrics.crawlMessages.Inc()
	g.srv.metrics.crawlBytes.Add(float64(len(msg)))

	if !strings.HasPrefix(msg, "*") {
		g.checkWhere()
		// Messages count as activity, except right after a spectator joined,
		// which makes crawl resend everything without the player doing anything.
		if time.Since(g.lastJoin) > 2*time.Second {
			g.noteActivity()
		}
		g.relay(msg)
		return
	}

	// A message for the server itself.
	var obj map[string]any
	if err := json.Unmarshal([]byte(msg[1:]), &obj); err != nil {
		g.logf("Invalid control message from the crawl process: %.80s", msg)
		return
	}
	str := func(key string) string {
		v, _ := obj[key].(string)
		return v
	}
	switch str("msg") {
	case "client_path":
		if g.clientPath == "" {
			g.clientPath = g.def.Format(str("path"), g.username)
			if v := str("version"); v != "" {
				g.crawlVersion = v
				g.logf("Crawl version: %s.", v)
			}
			for _, st := range g.seats {
				g.sendClient(st)
			}
		}
	case "flush_messages":
		g.queueMessages = true
		g.flush()
	case "dump":
		if base := g.def.MorgueURL(g.username); base != "" {
			url := base + str("filename")
			if str("type") == "command" {
				g.broadcast(encodeMessage("dump", M{"url": url}))
			} else {
				g.exitDumpURL = url
			}
		}
	case "exit_reason":
		g.exitReason = str("type")
		g.exitMessage = str("message")
	case "milestone":
		// The fields of a milestone or whereis update are in the message itself.
		g.directMilestones = true
		g.setWhere(obj)
	default:
		g.logf("Unknown message from the crawl process: %s", str("msg"))
	}
}

// onTerminalOutput handles what crawl (or a wrapper script) printed to the
// terminal. Until the socket comes up, JSON lines there are messages for the
// player.
func (g *Game) onTerminalOutput(lines []string) {
	g.noteActivity()
	if !g.useStdout {
		return
	}
	player := g.seats[g.playerSeat]
	for _, line := range lines {
		var js map[string]any
		if json.Unmarshal([]byte(line), &js) != nil {
			// Not a message (a crash report, say). Passing it on would make
			// the browser reject the whole frame.
			g.logf("Ignoring non-JSON output from Crawl process: %s", line)
			continue
		}
		if player != nil {
			player.send(line)
		}
	}
}

var crashReportRe = regexp.MustCompile(`crash report: (.*)`)
var parenRe = regexp.MustCompile(`\(([^)]+)\)`)

func stripExt(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// onCrawlStderr watches crawl's stderr for a crash report (see do_crash_dump
// in dbg-asrt.cc), to tell the player what happened and link the dump.
func (g *Game) onCrawlStderr(line string) {
	log.Printf("P%-5d ERR: %s", g.id, line)
	g.log.add("crawl", line)
	if strings.TrimSpace(line) != "" {
		g.lastStderr = line
	}
	morgueURL := g.def.MorgueURL(g.username)
	switch {
	case strings.HasPrefix(line, "ERROR"):
		// For example: ERROR in 'wizard.cc' at line 79: Intentional crash
		g.exitReason = "crash"
		if i := strings.LastIndex(line, ":"); i >= 0 {
			g.exitMessage = strings.TrimSpace(line[i+1:])
		}
	case strings.Contains(line, "crash report: "):
		g.exitReason = "crash"
		if m := crashReportRe.FindStringSubmatch(line); morgueURL != "" && m != nil && m[1] != "" {
			g.exitDumpURL = morgueURL + stripExt(m[1])
		}
	case strings.HasPrefix(line, "We crashed!"):
		// Before 0.19 the whole report was on this one line.
		g.exitReason = "crash"
		if m := parenRe.FindStringSubmatch(line); morgueURL != "" && m != nil {
			g.exitDumpURL = morgueURL + stripExt(m[1])
		}
	case strings.HasPrefix(line, "Writing crash info to"):
		// Before 0.15 this was all there was.
		g.exitReason = "crash"
		if morgueURL != "" {
			if i := strings.LastIndexAny(line, "/ "); i >= 0 {
				g.exitDumpURL = morgueURL + stripExt(strings.TrimSpace(line[i+1:]))
			}
		}
	}
}

// ---- input from the player's browser ----

func (g *Game) input(msg string, obj map[string]any, raw []byte) {
	switch {
	case msg == "force_terminate":
		answer := truthy(obj["answer"])
		g.forceAnswer = &answer
	case msg == "stop_stale_process_purge":
		g.cancelled = true
	case msg == "input" && g.term != nil:
		// Text typed into a prompt: key codes and/or a string.
		var text strings.Builder
		if codes, ok := obj["data"].([]any); ok {
			for _, c := range codes {
				if n, ok := c.(float64); ok {
					text.WriteRune(rune(n))
				}
			}
		}
		if s, ok := obj["text"].(string); ok {
			text.WriteString(s)
		}
		g.term.writeInput([]byte(text.String()))
	case g.conn != nil:
		g.sendToCrawl(raw)
	}
}

func (g *Game) sendToCrawl(data []byte) {
	if g.conn != nil && !g.conn.sendMessage(data) {
		g.srv.metrics.crawlDropped.Inc()
		g.logf("Game socket buffer full; skipping message ('%.50s')!", data)
	}
}

// ---- where the player is, for the lobby ----

func (g *Game) checkIdle() {
	if idle := g.IdleSeconds() > 30; idle != g.wasIdle {
		g.wasIdle = idle
		g.publish()
	}
}

func (g *Game) setWhere(newWhere map[string]any) {
	milestone := truthy(newWhere["milestone"])
	interesting := milestone || newWhere["status"] == "chargen"
	for _, key := range interestingInfo {
		if !reflect.DeepEqual(g.where[key], newWhere[key]) {
			interesting = true
		}
	}
	// A milestone sync message says nothing about where the player is.
	if newWhere["status"] != "milestone_only" {
		g.where = newWhere
	}
	if milestone {
		g.lastMilestone = newWhere
	}
	if interesting {
		g.publish()
	}
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	}
	return true
}

// parseWhereData parses a .where file: "key=value" fields separated by single
// colons, with a colon inside a value written as "::".
func parseWhereData(data string) map[string]any {
	where := map[string]any{}
	start := 0
	add := func(entry string) {
		if strings.TrimSpace(entry) == "" {
			return
		}
		field, value, _ := strings.Cut(entry, "=")
		where[strings.TrimSpace(field)] = strings.ReplaceAll(strings.TrimSpace(value), "::", ":")
	}
	for i := 1; i+1 < len(data); i++ {
		if data[i] == ':' && data[i-1] != ':' && data[i+1] != ':' {
			add(data[start:i])
			start = i + 1
		}
	}
	add(data[start:])
	return where
}

// checkWhere reads the player's .where file if it has changed, looking at
// most every few seconds. Crawl versions that report milestones over the
// socket don't need it.
func (g *Game) checkWhere() {
	if g.dglLess || g.directMilestones || time.Now().Before(g.nextWhereCheck) {
		return
	}
	g.nextWhereCheck = time.Now().Add(3 * time.Second)
	path := filepath.Join(g.configPath("morgue_path"), g.username+".where")
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().After(g.whereTime) {
		return
	}
	g.whereTime = time.Now()
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return
	}
	where := parseWhereData(string(data))
	if where["status"] == "active" || where["status"] == "saved" {
		g.setWhere(where)
	}
}

// publish tells the lobby how the game looks now.
func (g *Game) publish() {
	if !g.srv.cfg.DGLMode {
		return
	}
	e := &lobbyEntry{
		id:         g.id,
		username:   g.username,
		gameID:     g.def.ID,
		spectators: g.spectatorCount(),
		where:      g.where,
		milestone:  g.lastMilestone["milestone"],
	}
	if player := g.seats[g.playerSeat]; player != nil {
		e.adminOnly = player.restricted
	}
	if g.wasIdle {
		e.idleSince = time.Unix(0, g.lastActivity.Load())
	}
	g.srv.hub.publish(e)
}
