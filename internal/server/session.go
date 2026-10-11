package server

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/smw/geryon/internal/config"
	"github.com/smw/geryon/internal/userdb"
)

// Session is one browser connection. It runs on its own goroutine, which
// handles the browser's messages one at a time and owns every field below
// except where noted. Because nothing else waits on a session, its handlers
// are free to block: on the database, on a file, on a game's reply.
//
// A session is in exactly one place: the lobby, or a seat in a game (as the
// player or as a spectator).
type Session struct {
	srv      *Server
	id       int
	ws       *websocket.Conn
	remoteIP string
	deflate  bool
	out      *Outbox

	inbound chan []byte   // from the reader goroutine; closed when the browser is gone
	events  chan func()   // work from other goroutines, run on this one
	gone    chan struct{} // closed when run returns

	user    *userdb.UserInfo // nil while anonymous
	seat    *seatRef         // nil while in the lobby
	inLobby bool

	// Lobby save-slot text by game id. A nil entry means "not known, ask crawl".
	saveInfo map[string]*string

	receivedPong bool
	lastAction   time.Time
	connected    time.Time   // never changes
	reported     sessionInfo // what the hub was last told about this session

	// Written by the writer goroutine.
	totalBytes, sentBytes atomic.Int64
}

// seatRef is a session's place in a game.
type seatRef struct {
	game   *Game
	id     int
	player bool
	gameID string // the configured game being played, for the player
}

var lastSessionID atomic.Int64

func (srv *Server) newSession(ws *websocket.Conn, ip string) {
	s := &Session{
		srv:      srv,
		id:       int(lastSessionID.Add(1)),
		ws:       ws,
		remoteIP: ip,
		deflate:  ws.Subprotocol() != "no-compression",
		inbound:  make(chan []byte, 64),
		events:   make(chan func(), 64),
		gone:     make(chan struct{}),
		saveInfo: map[string]*string{},

		connected: time.Now(),
	}
	s.out = newOutbox(func() {
		srv.metrics.slowClients.Inc()
		ws.Close()
	})
	go s.writeLoop()
	go s.readLoop()
	go s.run()
}

func (s *Session) logf(format string, args ...any) {
	log.Printf("#%-5d "+format, append([]any{s.id}, args...)...)
}

func (s *Session) name() string {
	if s.user == nil {
		return "[Anon]"
	}
	return s.user.Username
}

func (s *Session) username() string {
	if s.user == nil {
		return ""
	}
	return s.user.Username
}

func (s *Session) isAdmin() bool { return s.user != nil && userdb.IsAdmin(s.user.Flags) }

// restricted is true for anonymous users and for accounts on hold.
func (s *Session) restricted() bool { return s.user == nil || userdb.AccountHold(s.user.Flags) }

func (s *Session) send(msg string, data M) { s.out.Send(frame(encodeMessage(msg, data))) }

// post runs f on the session's goroutine. It never blocks the caller: a
// session too far behind to take it is disconnected.
func (s *Session) post(f func()) {
	select {
	case s.events <- f:
	default:
		s.out.Close()
	}
}

func (s *Session) readLoop() {
	defer close(s.inbound)
	s.ws.SetReadLimit(8 << 20)
	for {
		_, data, err := s.ws.ReadMessage()
		if err != nil {
			return
		}
		s.srv.metrics.messagesReceived.Inc()
		select {
		case s.inbound <- data:
		case <-s.gone:
			return
		}
	}
}

// writeLoop sends frames to the browser. Unless the browser asked for
// "no-compression", each frame goes through this connection's deflate stream
// with the 00 00 FF FF sync marker cut off, which the browser puts back.
func (s *Session) writeLoop() {
	var compressed bytes.Buffer
	var deflater *flate.Writer
	if s.deflate {
		deflater, _ = flate.NewWriter(&compressed, flate.DefaultCompression)
	}
	m := s.srv.metrics
	rawBytes, wireBytes := m.bytesSent.WithLabelValues("raw"), m.bytesSent.WithLabelValues("wire")
	failed := false
	for frame := range s.out.frames {
		if failed {
			continue
		}
		m.framesSent.Inc()
		rawBytes.Add(float64(len(frame)))
		s.totalBytes.Add(int64(len(frame)))
		s.ws.SetWriteDeadline(time.Now().Add(60 * time.Second))
		var err error
		if deflater != nil {
			compressed.Reset()
			deflater.Write([]byte(frame))
			deflater.Flush()
			data := compressed.Bytes()
			data = data[:len(data)-4]
			s.sentBytes.Add(int64(len(data)))
			wireBytes.Add(float64(len(data)))
			err = s.ws.WriteMessage(websocket.BinaryMessage, data)
		} else {
			s.sentBytes.Add(int64(len(frame)))
			wireBytes.Add(float64(len(frame)))
			err = s.ws.WriteMessage(websocket.TextMessage, []byte(frame))
		}
		if err != nil {
			failed = true
			s.ws.Close() // the reader notices and ends the session
		}
	}
	if !failed {
		s.ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(5*time.Second))
	}
	s.ws.Close()
}

// run is the session's life.
func (s *Session) run() {
	srv := s.srv
	defer func() {
		if r := recover(); r != nil {
			s.logf("Internal error, closing connection: %v\n%s", r, debug.Stack())
			srv.metrics.failures.WithLabelValues("session").Inc()
		}
		close(s.gone)
		s.leaveSeat()
		srv.hub.removeSession(s)
		s.out.Close()
		total, sent := s.totalBytes.Load(), s.sentBytes.Load()
		ratio := "N/A"
		if total > 0 {
			ratio = fmt.Sprintf("%.2f%%", 100-100*float64(sent)/float64(total))
		}
		s.logf("Socket closed. (%s sent, compression ratio %s)", humaniseBytes(total), ratio)
	}()

	ok, refusal := srv.hub.addSession(s, srv.cfg.MaxConnections)
	if !ok {
		if refusal != "" {
			s.send("close", M{"reason": refusal})
		}
		return
	}
	compression := "on"
	if !s.deflate {
		compression = "off, client request"
	}
	s.logf("Socket opened from ip %s (compression: %s).", s.remoteIP, compression)
	srv.metrics.connections.Inc()

	s.lastAction = time.Now()
	s.send("ping", nil)
	alive := time.NewTicker(seconds(srv.cfg.ConnectionTimeout))
	defer alive.Stop()

	if srv.cfg.DGLMode {
		if srv.cfg.Autologin != "" {
			s.completeLogin(srv.cfg.Autologin)
		}
		s.enterLobby()
	} else {
		s.startGame(config.DGLLessGame(), "game", true)
	}

	for {
		s.report()
		select {
		case data, open := <-s.inbound:
			if !open {
				return
			}
			s.handle(data)
		case f := <-s.events:
			f()
		case <-alive.C:
			s.checkAlive()
		}
	}
}

// report tells the hub what this session is doing, when that has changed.
func (s *Session) report() {
	info := sessionInfo{user: s.username(), admin: s.isAdmin()}
	switch {
	case s.seat != nil && s.seat.player:
		info.state, info.target = "playing", s.seat.gameID
	case s.seat != nil:
		info.state, info.target = "watching", s.seat.game.username
	case s.inLobby:
		info.state = "lobby"
	}
	if info != s.reported {
		s.reported = info
		s.srv.hub.setSessionInfo(s, info)
	}
}

// announce shows a server-wide announcement to a session that is in a game:
// in its chat window, and for the player also in crawl's message log.
func (s *Session) announce(text string) {
	seat := s.seat
	if seat == nil {
		return
	}
	s.send("server_announcement", M{"text": text})
	if seat.player {
		note := []byte(encodeMessage("server_announcement", M{"content": text}))
		seat.game.do(func() { seat.game.sendToCrawl(note) })
	}
}

// kick ends the session on an administrator's instruction.
func (s *Session) kick(reason string) {
	s.logf("Disconnecting %s: %s", s.name(), reason)
	s.send("close", M{"reason": reason})
	s.out.Close()
	s.leaveSeat()
}

// checkAlive runs every connection_timeout seconds: the browser must have
// answered the last ping, and must not have sat idle for too long.
func (s *Session) checkAlive() {
	cfg := s.srv.cfg
	switch {
	case !s.receivedPong:
		s.logf("Connection timed out.")
		s.out.Close()
		return
	case s.seat != nil && s.seat.player:
		if float64(s.seat.game.IdleSeconds()) > cfg.MaxIdleTime {
			s.logf("Stopping crawl after idle time limit.")
			s.seat.game.Stop()
		}
	case s.inLobby && !s.isAdmin() && cfg.MaxLobbyIdleTime > 0:
		if time.Since(s.lastAction) > seconds(max(cfg.MaxLobbyIdleTime, cfg.ConnectionTimeout)) {
			s.logf("Lobby connection timed out.")
			s.out.Close()
			return
		}
	}
	s.receivedPong = false
	s.send("ping", nil)
}

// shutdown tells the browser the server is stopping and ends the session. A
// game being played is told to save.
func (s *Session) shutdown() {
	playing := s.seat != nil && s.seat.player
	s.logf("Shutting down user %s", s.name())
	msg, err := s.srv.renderTemplate("shutdown.html", map[string]any{"game.is_running()": playing})
	if err != nil {
		msg = "The server is shutting down."
	}
	s.send("close", M{"reason": msg})
	s.out.Close()
	s.leaveSeat()
}

// ---- incoming messages ----

func (s *Session) handle(raw []byte) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		s.logf("Error while handling JSON message ('%.50s')!", raw)
		return
	}
	str := func(key string) string {
		v, _ := obj[key].(string)
		return v
	}
	msg := str("msg")
	if msg != "pong" {
		s.lastAction = time.Now()
	}
	switch msg {
	case "pong":
		s.receivedPong = true
	case "login":
		s.login(str("username"), str("password"))
	case "token_login":
		s.tokenLogin(str("cookie"))
	case "set_login_cookie":
		s.setLoginCookie()
	case "forget_login_cookie":
		s.srv.hub.forgetToken(str("cookie"))
	case "register":
		s.register(str("username"), str("password"), str("email"))
	case "start_change_email":
		s.send("start_change_email", M{"email": s.email()})
	case "change_email":
		s.changeEmail(str("email"))
	case "start_change_password":
		s.send("start_change_password", nil)
	case "change_password":
		s.changePassword(str("cur_password"), str("new_password"))
	case "play":
		s.play(str("game_id"))
	case "watch":
		s.watch(str("username"))
	case "go_lobby":
		s.goLobby("")
	case "go_admin":
		s.goLobby("")
		s.send("go_admin", nil)
	case "chat_msg":
		s.chat(str("text"))
	case "get_rc":
		s.getRC(str("game_id"))
	case "set_rc":
		s.setRC(str("game_id"), str("contents"))
	case "admin_announce":
		// From the admin panel in the browser client, for accounts flagged admin.
		if !s.isAdmin() {
			s.logf("Non-admin user %s attempted an announcement.", s.name())
			return
		}
		s.logf("User %s sent serverwide announcement: %s", s.name(), str("text"))
		s.srv.announce(str("text"))
		s.send("admin_log", M{"text": "Announcement made ('" + str("text") + "')"})
	case "forgot_password", "reset_password", "admin_pw_reset", "admin_pw_reset_clear":
		s.logf("Message '%s' from %s is not supported by this server.", msg, s.name())
	default:
		// Anything else from the player is for the game.
		if seat := s.seat; seat != nil && seat.player {
			seat.game.do(func() { seat.game.input(msg, obj, raw) })
		}
	}
}

// ---- accounts ----

func (s *Session) email() string {
	if s.user == nil {
		return ""
	}
	return s.user.Email
}

// refreshUser rereads the user's record, so that a ban or a hold applied
// since login takes effect. It returns false, having logged the user out, if
// the account is banned or gone.
func (s *Session) refreshUser() bool {
	if s.user == nil {
		return true
	}
	u := s.srv.db.GetUserInfo(s.user.Username)
	if u == nil || userdb.IsBanned(u.Flags) {
		s.logf("[Account] Logging out user %s: Account is disabled.", s.user.Username)
		s.user = nil
		s.leaveSeat()
		s.send("go_lobby", nil)
		s.enterLobby()
		s.send("logout", M{"reason": "Account is disabled."})
		return false
	}
	if userdb.AccountHold(s.user.Flags) && !userdb.AccountHold(u.Flags) {
		s.logf("[Account] Hold cleared for user %s (IP: %s)", u.Username, s.remoteIP)
	}
	s.user = u
	return true
}

// completeLogin logs the session in as username, whose credentials have
// already been checked.
func (s *Session) completeLogin(username string) {
	u := s.srv.db.GetUserInfo(username)
	if u == nil || userdb.IsBanned(u.Flags) {
		s.logf("[Account] Failed login for user %s: Account is disabled.", username)
		s.send("login_fail", M{"reason": "Account is disabled."})
		return
	}

	// The init program creates the player's rc file and directories.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, s.srv.cfg.InitPlayerProgram, u.Username).Run(); err != nil {
		s.logf("User initialization returned an error for user %s: %v", u.Username, err)
		s.send("close", M{"reason": "Could not initialize your rc and morgue!<br>" +
			"This probably means there is something wrong with the server configuration."})
		s.out.Close()
		return
	}

	s.user = u
	msgs := []string{encodeMessage("login_success", M{"username": u.Username, "admin": s.isAdmin()})}
	if s.restricted() {
		msgs = append(msgs, encodeMessage("set_account_hold", nil))
	}
	s.out.Send(frame(msgs...))

	switch {
	case s.seat != nil && !s.seat.player:
		// A spectator who logs in shows up by name.
		seat, info := s.seat, s.seatInfo(0)
		seat.game.do(func() { seat.game.updateIdentity(seat.id, info) })
	case s.inLobby && s.isAdmin():
		s.enterLobby() // again: admins also see restricted games
	case s.inLobby:
		s.sendLobbyHTML()
	}
}

func (s *Session) login(username, password string) {
	ok, realName, reason := s.srv.db.PasswdMatch(username, password)
	s.srv.metrics.countLogin("password", ok)
	switch {
	case ok:
		s.logf("User %s logging in from %s.", realName, s.remoteIP)
		s.completeLogin(realName)
	case reason != "":
		s.logf("Failed login for user %s (IP: %s): %s", realName, s.remoteIP, reason)
		s.send("login_fail", M{"reason": reason})
	default:
		s.logf("Failed login for user %s (IP: %s).", username, s.remoteIP)
		s.send("login_fail", nil)
	}
}

func (s *Session) tokenLogin(cookie string) {
	username, ok := s.srv.hub.redeemToken(cookie)
	s.srv.metrics.countLogin("token", ok)
	if !ok {
		s.logf("Wrong login token for user %s. (IP: %s)", username, s.remoteIP)
		s.send("login_fail", nil)
		return
	}
	s.logf("User %s logging in from %s (via token).", username, s.remoteIP)
	s.completeLogin(username)
}

func (s *Session) setLoginCookie() {
	if s.user == nil {
		return
	}
	days := s.srv.cfg.LoginTokenLifetime
	cookie, err := s.srv.hub.issueToken(s.user.Username, time.Duration(days*24*float64(time.Hour)))
	if err != nil {
		s.logf("Could not generate a login token: %v", err)
		return
	}
	s.send("login_cookie", M{"cookie": cookie, "expires": days})
}

func (s *Session) register(username, password, email string) {
	msg := s.srv.db.Register(username, password, email)
	s.srv.metrics.registrations.WithLabelValues(result(msg == "")).Inc()
	if msg != "" {
		s.logf("[Account] Registration attempt failed for user %s (IP: %s): %s", username, s.remoteIP, msg)
		s.send("register_fail", M{"reason": msg})
		return
	}
	s.logf("[Account] Registered user %s (IP: %s, email: %s).", username, s.remoteIP, email)
	s.completeLogin(strings.TrimSpace(username))
}

// accountChange checks that the session may change its account, and reports
// why not through fail.
func (s *Session) accountChange(what string, fail func(string)) bool {
	switch {
	case s.user == nil:
		fail("You need to log in to change your " + what + ".")
	case !s.refreshUser():
		fail("Account is disabled")
	case s.restricted():
		fail("Your account is restricted; change " + what + " unavailable.")
	default:
		return true
	}
	return false
}

func (s *Session) changePassword(current, replacement string) {
	fail := func(reason string) { s.send("change_password_fail", M{"reason": reason}) }
	if !s.accountChange("password", fail) {
		return
	}
	if ok, _, _ := s.srv.db.PasswdMatch(s.user.Username, current); !ok {
		s.logf("Non-matching current password during password change for %s", s.user.Username)
		fail("Your password didn't match.")
		return
	}
	if msg := s.srv.db.ChangePassword(s.user.ID, replacement); msg != "" {
		s.logf("Failed to change password for %s: %s", s.user.Username, msg)
		fail(msg)
		return
	}
	s.logf("User %s changed password.", s.user.Username)
	s.send("change_password_done", nil)
}

func (s *Session) changeEmail(email string) {
	fail := func(reason string) { s.send("change_email_fail", M{"reason": reason}) }
	if !s.accountChange("email", fail) {
		return
	}
	if msg := s.srv.db.ChangeEmail(s.user.ID, email); msg != "" {
		s.logf("Failed to change email for %s: %s", s.user.Username, msg)
		fail(msg)
		return
	}
	s.user.Email = email
	s.logf("User %s changed email to %s.", s.user.Username, email)
	s.send("change_email_done", M{"email": email})
}

func (s *Session) rcfilePath(gameID string) string {
	game := s.srv.gameSet().Get(gameID)
	if game == nil || s.user == nil {
		return ""
	}
	return filepath.Join(game.Str("rcfile_path", s.user.Username), s.user.Username+".rc")
}

func (s *Session) getRC(gameID string) {
	path := s.rcfilePath(gameID)
	if path == "" {
		return
	}
	contents, _ := os.ReadFile(path) // a missing rc file reads as empty
	s.send("rcfile_contents", M{"game_id": gameID, "contents": string(contents)})
}

func (s *Session) setRC(gameID, contents string) {
	path := s.rcfilePath(gameID)
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		s.logf("Couldn't save rcfile for %s: %v", s.user.Username, err)
	}
}

// ---- moving between the lobby and games ----

// seatInfo describes this session to a game.
func (s *Session) seatInfo(epoch uint64) seatInfo {
	return seatInfo{
		sess:       s,
		out:        s.out,
		epoch:      epoch,
		name:       s.username(),
		admin:      s.isAdmin(),
		restricted: s.restricted(),
	}
}

// leaveSeat gives up the session's place in a game, if it has one. The
// player leaving stops the game.
func (s *Session) leaveSeat() {
	seat := s.seat
	if seat == nil {
		return
	}
	s.seat = nil
	if seat.player {
		seat.game.Stop()
	} else {
		s.logf("%s stopped watching %s.", s.name(), seat.game.username)
		seat.game.do(func() { seat.game.leave(seat.id) })
	}
}

func (s *Session) leaveLobby() {
	if s.inLobby {
		s.srv.hub.unsubscribe(s)
		s.inLobby = false
	}
}

// enterLobby puts the session in the lobby and sends it the lobby's contents.
func (s *Session) enterLobby() {
	s.seat, s.inLobby = nil, true
	s.srv.hub.subscribe(s, s.out.Advance(), s.isAdmin())
	s.sendLobbyHTML()
	if s.isAdmin() {
		s.out.Send(frame(
			encodeMessage("admin_log", M{"text": "Webtiles (" + s.srv.version() + ") running the Go server"}),
			encodeMessage("admin_log", M{"text": s.srv.hub.describe()})))
	}
}

// goLobby is the browser asking for the lobby, or the server sending it
// there with a message to show above the login box.
func (s *Session) goLobby(message string) {
	if !s.srv.cfg.DGLMode {
		return
	}
	if message != "" {
		s.send("auth_error", M{"reason": message})
	}
	switch {
	case s.seat != nil && s.seat.player:
		// Ask the game to save. The session stays seated to see it finish, and
		// seatClosed takes it to the lobby.
		s.seat.game.Stop()
	case s.seat != nil:
		s.leaveSeat()
		s.send("go_lobby", nil)
		s.enterLobby()
	default:
		s.send("go_lobby", nil)
		if !s.inLobby {
			s.enterLobby()
		}
	}
}

func (s *Session) gameAllowed(game *config.Game) bool {
	return !s.restricted() || game.Bool("allowed_with_hold", true)
}

func (s *Session) play(gameID string) {
	game := s.srv.gameSet().Get(gameID)
	switch {
	case game == nil:
		s.goLobby("")
		return
	case s.user == nil:
		if s.seat != nil {
			s.leaveSeat()
			s.enterLobby()
		}
		s.send("login_required", M{"game": game.Str("name", "")})
		return
	case s.seat != nil && s.seat.player:
		// A repeated request for the same game is normal when logging in with a
		// cookie; ignore it.
		if s.seat.gameID != gameID {
			s.goLobby("")
		}
		return
	}
	if !s.refreshUser() {
		return
	}
	if !s.gameAllowed(game) {
		s.goLobby("")
		return
	}
	s.leaveSeat()
	s.invalidateSaveSlot(gameID)
	s.startGame(game, s.user.Username, false)
}

func (s *Session) startGame(def *config.Game, username string, dglLess bool) {
	s.leaveLobby()
	g := newGame(s.srv, def, username, dglLess, s.seatInfo(s.out.Advance()))
	s.seat = &seatRef{game: g, id: g.playerSeat, player: true, gameID: def.ID}
	go g.run()
}

func (s *Session) watch(username string) {
	cfg := s.srv.cfg
	if s.seat != nil && s.seat.player {
		s.leaveSeat() // stops the game being played
	}
	if !s.refreshUser() {
		return
	}
	if s.user != nil && s.restricted() {
		s.goLobby("Your account is restricted; spectating unavailable")
		return
	}
	if s.user == nil && !cfg.AllowAnonSpectate {
		s.goLobby("Anonymous spectating disabled")
		return
	}
	g := s.srv.hub.findGame(username)
	if g == nil {
		s.goLobby("")
		return
	}
	if s.seat != nil && s.seat.game == g {
		return
	}
	s.leaveSeat()
	s.leaveLobby()
	epoch := s.out.Advance()
	seatID, ok, refusal := g.join(s.seatInfo(epoch))
	if !ok {
		s.goLobby(refusal)
		return
	}
	s.logf("%s started watching %s (P%d).", s.name(), g.username, g.id)
	s.seat = &seatRef{game: g, id: seatID}
	s.sendSpectatorOptions(g, epoch)
}

// sendSpectatorOptions sends a spectator the options from their own rc file,
// as crawl reads them, so the game renders with the spectator's settings.
func (s *Session) sendSpectatorOptions(g *Game, epoch uint64) {
	def := g.def
	if s.user == nil || s.srv.gameSet().Get(def.ID) == nil || !def.Bool("send_json_options", true) {
		return
	}
	name := s.user.Username
	call := append(def.CallBase(), "-name", g.username, "-rc", s.rcfilePath(def.ID))
	call = append(call, def.List("options", name)...)
	call = append(call, "-print-webtiles-options")
	cwd := def.Str("cwd", name)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, call[0], call[1:]...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			// Exit status 1 is an old crawl without the option; stay quiet.
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				s.logf("Error while getting JSON options: %v", err)
			}
			return
		}
		s.out.SendAt(epoch, frame(`{"msg":"options","watcher":true,"options":`+strings.TrimSpace(string(out))+`}`))
	}()
}

// seatClosed is a game telling the session its seat is gone: the game ended,
// or the spectator was removed. message, if any, is shown in the lobby.
func (s *Session) seatClosed(g *Game, message string, failed bool) {
	seat := s.seat
	if seat == nil || seat.game != g {
		return // already left
	}
	s.seat = nil
	switch {
	case s.srv.hub.isShuttingDown():
		s.out.Close()
	case !s.srv.cfg.DGLMode:
		if failed {
			s.out.Close() // don't spin on a crawl that cannot start
		} else {
			s.startGame(config.DGLLessGame(), "game", true)
		}
	default:
		if message != "" {
			s.send("auth_error", M{"reason": message})
		}
		if seat.player {
			s.invalidateSaveSlot(seat.gameID)
		}
		s.send("go_lobby", nil)
		s.enterLobby()
	}
}

func (s *Session) chat(text string) {
	seat := s.seat
	if seat == nil {
		return
	}
	if limit := s.srv.cfg.MaxChatLength; limit > 0 {
		limit = max(100, limit)
		if r := []rune(text); len(r) >= limit {
			text = string(r[:limit-5]) + "[...]"
		}
	}
	seat.game.do(func() { seat.game.chat(seat.id, text) })
}

// ---- lobby HTML ----

// invalidateSaveSlot forgets cached save info for a game. Game modes can share
// a save slot, so anything whose text could be stale because of that goes too.
func (s *Session) invalidateSaveSlot(slot string) {
	if cur := s.saveInfo[slot]; cur != nil {
		check := "[slot full]"
		if *cur == "" {
			check = ""
		}
		for g, v := range s.saveInfo {
			if v != nil && *v == check {
				s.saveInfo[g] = nil
			}
		}
	}
	s.saveInfo[slot] = nil
}

// updateSaveInfo records one binary's `-save-json` output: a map from game
// mode to a description of the save in that mode's slot.
func (s *Session) updateSaveInfo(binaryKey string, output []byte, ok bool) {
	games := s.srv.gameSet()
	set := func(g, text string) { s.saveInfo[g] = &text }

	var data map[string]struct {
		Loadable  bool   `json:"loadable"`
		GameType  string `json:"game_type"`
		ShortDesc string `json:"short_desc"`
	}
	if !ok || json.Unmarshal(output, &data) != nil {
		for _, g := range games.BinaryGames[binaryKey] {
			set(g, "")
		}
		return
	}
	for _, g := range games.BinaryGames[binaryKey] {
		mode, haveMode := games.Modes[g]
		save, haveSave := data[mode]
		switch {
		case !haveMode || !haveSave || !games.Get(g).Bool("show_save_info", false):
			set(g, "")
		case !save.Loadable:
			set(g, "[playing]") // the save in this slot is in use
		case mode == save.GameType:
			set(g, "["+save.ShortDesc+"]")
		default:
			set(g, "[slot full]") // a save of another game type that shares the slot
		}
	}
}

// collectSaveInfo asks each crawl binary about the player's saves, for the
// games whose cached info is missing, then calls done on this goroutine.
func (s *Session) collectSaveInfo(done func()) {
	games := s.srv.gameSet()
	username := s.user.Username
	type check struct {
		key    string
		call   []string
		cwd    string
		output []byte
		ok     bool
	}
	var checks []*check
	for _, key := range games.BinaryKeys {
		ids := games.BinaryGames[key]
		if _, haveMode := games.Modes[ids[0]]; !haveMode {
			continue
		}
		showAny, stale := false, false
		for _, g := range ids {
			showAny = showAny || games.Get(g).Bool("show_save_info", false)
			stale = stale || s.saveInfo[g] == nil
		}
		if !showAny {
			for _, g := range ids {
				empty := ""
				s.saveInfo[g] = &empty
			}
			continue
		}
		if !stale {
			continue
		}
		game := games.Get(ids[0])
		checks = append(checks, &check{key: key, cwd: game.Str("cwd", username),
			call: append(game.CallBase(), "-save-json", username)})
	}

	go func() {
		for _, c := range checks {
			// Run where the game runs: crawl looks for saves relative to it.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			cmd := exec.CommandContext(ctx, c.call[0], c.call[1:]...)
			cmd.Dir = c.cwd
			out, err := cmd.Output()
			cancel()
			c.output, c.ok = out, err == nil
			if err != nil {
				log.Printf("Save info check for '%s' failed", c.key)
			}
		}
		s.post(func() {
			if s.user == nil || s.user.Username != username {
				return
			}
			for _, c := range checks {
				s.updateSaveInfo(c.key, c.output, c.ok)
			}
			done()
		})
	}()
}

// gameLinksHTML is the "Play now:" block of the lobby. It produces the same
// markup as templates/game_links.html, whose loops are beyond the template
// subset this server renders.
func (s *Session) gameLinksHTML() string {
	username := s.user.Username
	var b strings.Builder
	b.WriteString("Play now:\n")
	rcs := map[string]bool{}
	for _, g := range s.srv.gameSet().Games {
		if !s.gameAllowed(g) {
			continue
		}
		save := ""
		if v := s.saveInfo[g.ID]; v != nil {
			save = *v
		}
		disabled := save == "[slot full]"
		id := html.EscapeString(g.ID)
		name := html.EscapeString(g.Str("name", username))
		link := `<a href="#play-` + id + `">`

		if disabled {
			b.WriteString(`<span class="fg7">`)
		} else {
			b.WriteString(`<span>`)
		}
		rcPath := g.Str("rcfile_path", username)
		firstForRC := !rcs[rcPath]
		switch {
		case g.Has("separator"):
			b.WriteString(g.Str("separator", username))
		case firstForRC:
			b.WriteString("<br>")
		default:
			b.WriteString(" | ")
		}
		if save == "" {
			b.WriteString(link + name + "</a>")
		} else {
			b.WriteString(name + " <span>")
			if disabled {
				b.WriteString(html.EscapeString(save))
			} else {
				b.WriteString(link + html.EscapeString(save) + "</a>")
			}
			b.WriteString("</span>")
		}
		if firstForRC {
			rcs[rcPath] = true
			b.WriteString(` <a href="javascript:" class="edit_rc_link" data-game_id="` + id + `">(edit rc)</a>`)
		}
		b.WriteString("</span>\n")
	}
	return b.String()
}

// sendLobbyHTML sends the banner and, to a logged-in user, the list of games
// to play with a note of the saved game in each.
func (s *Session) sendLobbyHTML() {
	banner, err := s.srv.renderTemplate("banner.html", map[string]any{"username": s.username()})
	if err != nil {
		s.logf("Error rendering banner.html: %v", err)
	}
	bannerMsg := encodeMessage("html", M{"id": "banner", "content": banner})
	if s.user == nil {
		s.out.Send(frame(bannerMsg))
		return
	}
	sendGameLinks := func() {
		if s.inLobby {
			s.out.Send(frame(bannerMsg, encodeMessage("set_game_links", M{"content": s.gameLinksHTML()})))
		}
	}
	// On the first visit send the links straight away, without save info, so
	// the player sees something if the crawl binaries are slow to answer.
	if len(s.saveInfo) == 0 {
		for _, g := range s.srv.gameSet().Games {
			s.saveInfo[g.ID] = nil
		}
		sendGameLinks()
	}
	s.collectSaveInfo(sendGameLinks)
}

func humaniseBytes(n int64) string {
	units := []string{"bytes", "kilobytes", "megabytes", "gigabytes"}
	v, i := float64(n), 0
	for ; v >= 1000 && i < len(units)-1; i++ {
		v /= 1000
	}
	if i == 0 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%.2f %s", v, units[i])
}
