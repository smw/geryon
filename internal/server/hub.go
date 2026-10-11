package server

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

// Hub is what the whole server shares: who is connected, which games are
// running, what the lobby shows, and the login tokens. It is a plain locked
// structure; nothing in it blocks or calls back into a game or a session.
type Hub struct {
	mu sync.Mutex

	sessions     map[*Session]sessionInfo // each session's state, as it last reported it
	shuttingDown bool

	games       map[int]*Game // started games, which can be found and watched
	lastGameID  int
	activeGames int // includes games still starting up

	// The lobby: one entry per running game, as last published by the game,
	// and the connections currently looking at it.
	lobby map[int]*lobbyEntry
	dirty map[int]struct{}
	subs  map[*Session]subscriber

	tokens map[loginToken]time.Time
}

type subscriber struct {
	epoch uint64
	admin bool
}

type loginToken struct{ token, username string }

// lobbyEntry is a game as the lobby shows it.
type lobbyEntry struct {
	id         int
	username   string
	gameID     string
	adminOnly  bool // the player's account is on hold
	spectators int
	idleSince  time.Time // zero while the player is active
	where      map[string]any
	milestone  any
}

// The fields of a game's location that the lobby shows.
var interestingInfo = []string{"xl", "char", "place", "turn", "dur", "god", "title"}

func (e *lobbyEntry) idleTime() int {
	if e.idleSince.IsZero() {
		return 0
	}
	return int(time.Since(e.idleSince).Seconds())
}

func (e *lobbyEntry) message() string {
	username := e.username
	if e.adminOnly {
		username = "[account hold] " + username
	}
	m := M{
		"id":              e.id,
		"username":        username,
		"spectator_count": e.spectators,
		"idle_time":       e.idleTime(),
		"game_id":         e.gameID,
	}
	for _, key := range interestingInfo {
		if v, ok := e.where[key]; ok {
			m[key] = v
		}
	}
	if truthy(e.milestone) {
		m["milestone"] = e.milestone
	}
	return encodeMessage("lobby_entry", m)
}

func newHub() *Hub {
	return &Hub{
		sessions: map[*Session]sessionInfo{},
		games:    map[int]*Game{},
		lobby:    map[int]*lobbyEntry{},
		dirty:    map[int]struct{}{},
		subs:     map[*Session]subscriber{},
		tokens:   map[loginToken]time.Time{},
	}
}

// ---- sessions ----

// addSession admits a connection, unless the server is full or stopping.
func (h *Hub) addSession(s *Session, limit int) (ok bool, refusal string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.shuttingDown:
		return false, ""
	case len(h.sessions) >= limit:
		return false, "The maximum number of connections has been reached, sorry :("
	}
	h.sessions[s] = sessionInfo{}
	return true, ""
}

func (h *Hub) removeSession(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, s)
	delete(h.subs, s)
}

func (h *Hub) isShuttingDown() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shuttingDown
}

// shutdown stops new connections and returns the current ones.
func (h *Hub) shutdown() []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shuttingDown = true
	out := make([]*Session, 0, len(h.sessions))
	for s := range h.sessions {
		out = append(out, s)
	}
	return out
}

func (h *Hub) counts() (sessions, games int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions), h.activeGames
}

// ---- games and the lobby ----

// newGameID numbers a game that is about to start, and counts it as active
// until removeGame.
func (h *Hub) newGameID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastGameID++
	h.activeGames++
	return h.lastGameID
}

// addGame makes a started game visible: it can be found and watched.
func (h *Hub) addGame(g *Game) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.games[g.id] = g
}

// findGame returns the oldest running game of the named player.
func (h *Hub) findGame(username string) *Game {
	h.mu.Lock()
	defer h.mu.Unlock()
	var found *Game
	for _, g := range h.games {
		if strings.EqualFold(g.username, username) && (found == nil || g.id < found.id) {
			found = g
		}
	}
	return found
}

// publish records how a game should appear in the lobby. Viewers are told at
// the next flushLobby, so a burst of changes costs them one update.
func (h *Hub) publish(e *lobbyEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, running := h.games[e.id]; running {
		h.lobby[e.id] = e
		h.dirty[e.id] = struct{}{}
	}
}

// removeGame takes an ended game out of the lobby at once.
func (h *Hub) removeGame(g *Game, reason, message, dump string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activeGames--
	delete(h.games, g.id)
	delete(h.dirty, g.id)
	if _, shown := h.lobby[g.id]; !shown {
		return
	}
	delete(h.lobby, g.id)
	msg := frame(encodeMessage("lobby_remove", M{"id": g.id, "reason": orNull(reason),
		"message": orNull(message), "dump": orNull(dump)}))
	for s, sub := range h.subs {
		s.out.SendAt(sub.epoch, msg)
	}
}

func (h *Hub) sortedLobby() []*lobbyEntry {
	entries := make([]*lobbyEntry, 0, len(h.lobby))
	for _, e := range h.lobby {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	return entries
}

// subscribe puts a connection in the lobby and sends it the current list.
func (h *Hub) subscribe(s *Session, epoch uint64, admin bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[s] = subscriber{epoch: epoch, admin: admin}
	msgs := []string{encodeMessage("lobby_clear", nil)}
	for _, e := range h.sortedLobby() {
		if !e.adminOnly || admin {
			msgs = append(msgs, e.message())
		}
	}
	msgs = append(msgs, encodeMessage("lobby_complete", nil))
	s.out.SendAt(epoch, frame(msgs...))
}

func (h *Hub) unsubscribe(s *Session) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
}

// flushLobby sends the lobby's viewers the games that changed.
func (h *Hub) flushLobby() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.dirty) == 0 {
		return
	}
	var public, restricted []string
	for id := range h.dirty {
		if e := h.lobby[id]; e == nil {
		} else if e.adminOnly {
			restricted = append(restricted, e.message())
		} else {
			public = append(public, e.message())
		}
	}
	clear(h.dirty)
	for s, sub := range h.subs {
		msgs := public
		if sub.admin {
			msgs = append(append([]string{}, public...), restricted...)
		}
		if len(msgs) > 0 {
			s.out.SendAt(sub.epoch, frame(msgs...))
		}
	}
}

// statusLobby is the lobby as /status/lobby/ reports it.
func (h *Hub) statusLobby() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	games := []map[string]any{}
	for _, e := range h.sortedLobby() {
		if e.adminOnly {
			continue
		}
		game := map[string]any{
			"name":      e.username,
			"game_id":   e.gameID,
			"idle_time": e.idleTime(),
			"viewers":   e.spectators,
			"milestone": "",
		}
		for _, k := range []string{"v", "vlong", "tiles", "race", "cls", "char", "xl",
			"title", "place", "god", "turn", "dur"} {
			game[k] = ""
			if v, ok := e.where[k]; ok {
				game[k] = v
			}
		}
		if truthy(e.milestone) {
			game["milestone"] = e.milestone
		}
		games = append(games, game)
	}
	return games
}

// describe summarises who is connected, for logs and the admin panel.
func (h *Hub) describe() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var players []string
	for _, g := range h.games {
		players = append(players, g.username)
	}
	sort.Strings(players)
	summary := fmt.Sprintf("%d connections (%d in lobby), %d games", len(h.sessions), len(h.subs), len(h.games))
	if len(players) > 0 {
		summary += "; Playing: " + strings.Join(players, ", ")
	}
	return summary
}

// ---- login tokens ----

// A login cookie is "<username>%20<token>". Tokens are single use: logging in
// with one consumes it, and the browser then asks for a fresh one.
func parseLoginCookie(cookie string) loginToken {
	username, token, _ := strings.Cut(cookie, "%20")
	return loginToken{token: token, username: username}
}

func (h *Hub) issueToken(username string, lifetime time.Duration) (cookie string, err error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	tok := loginToken{token: n.String(), username: username}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokens[tok] = time.Now().Add(lifetime)
	return tok.username + "%20" + tok.token, nil
}

// redeemToken consumes a login cookie and returns the user it logs in.
func (h *Hub) redeemToken(cookie string) (username string, ok bool) {
	tok := parseLoginCookie(cookie)
	h.mu.Lock()
	defer h.mu.Unlock()
	expires, known := h.tokens[tok]
	if !known || tok.token == "" || time.Now().After(expires) {
		return tok.username, false
	}
	delete(h.tokens, tok)
	return tok.username, true
}

func (h *Hub) forgetToken(cookie string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.tokens, parseLoginCookie(cookie))
}

func (h *Hub) purgeTokens() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	for tok, expires := range h.tokens {
		if now.After(expires) {
			delete(h.tokens, tok)
		}
	}
}
