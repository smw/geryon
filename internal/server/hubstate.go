package server

import (
	"sort"
	"strings"
	"time"
)

// What the hub knows about sessions and games beyond the lobby, for the
// admin pages and the metrics. Sessions report their own state here; nothing
// reads a session's or a game's private fields from outside.

// sessionInfo is a session's state as it last reported it.
type sessionInfo struct {
	user   string // "" while anonymous
	admin  bool
	state  string // "lobby", "playing" or "watching"; "" in between
	target string // playing: the game id; watching: the player's name
}

// setSessionInfo records a session's current state.
func (h *Hub) setSessionInfo(s *Session, info sessionInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, connected := h.sessions[s]; connected {
		h.sessions[s] = info
	}
}

type hubStats struct {
	connections map[string]int // by state
	games       map[string]int // by game id
	spectators  int
	idle        int
}

func (h *Hub) stats() hubStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := hubStats{connections: map[string]int{}, games: map[string]int{}}
	for _, info := range h.sessions {
		st.connections[info.state]++
	}
	st.spectators = st.connections["watching"]
	for _, g := range h.games {
		st.games[g.def.ID]++
		if g.IdleSeconds() > 30 {
			st.idle++
		}
	}
	return st
}

// sessionView and gameView are rows of the admin pages.
type sessionView struct {
	ID        int
	User      string
	Admin     bool
	IP        string
	State     string
	Target    string
	Connected time.Time
}

type gameView struct {
	ID         int
	Player     string
	GameID     string
	Where      string // "L8 MuIE, D:6", when crawl has reported it
	Milestone  string
	Spectators int
	Idle       int // seconds
	Started    time.Time
	Binary     string
	PID        int
}

func (h *Hub) sessionViews() []sessionView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]sessionView, 0, len(h.sessions))
	for s, info := range h.sessions {
		out = append(out, sessionView{ID: s.id, User: info.user, Admin: info.admin, IP: s.remoteIP,
			State: info.state, Target: info.target, Connected: s.connected})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *Hub) gameViews() []gameView {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]gameView, 0, len(h.games))
	for _, g := range h.games {
		v := gameView{ID: g.id, Player: g.username, GameID: g.def.ID, Idle: g.IdleSeconds(),
			Started: g.created, Binary: g.def.Str("crawl_binary", ""), PID: int(g.pid.Load())}
		if e := h.lobby[g.id]; e != nil {
			v.Spectators = e.spectators
			str := func(key string) string {
				if s, ok := e.where[key].(string); ok {
					return s
				}
				return ""
			}
			if str("xl") != "" {
				v.Where = "L" + str("xl") + " " + str("char") + ", " + str("place")
			}
			v.Milestone, _ = e.milestone.(string)
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (h *Hub) sessionByID(id int) *Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.sessions {
		if s.id == id {
			return s
		}
	}
	return nil
}

func (h *Hub) gameByID(id int) *Game {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.games[id]
}

func (h *Hub) allSessions() []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Session, 0, len(h.sessions))
	for s := range h.sessions {
		out = append(out, s)
	}
	return out
}

// sessionsOf returns the connections logged in as username.
func (h *Hub) sessionsOf(username string) []*Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*Session
	for s, info := range h.sessions {
		if info.user != "" && strings.EqualFold(info.user, username) {
			out = append(out, s)
		}
	}
	return out
}
