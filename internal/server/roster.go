package server

import (
	"fmt"
	"html"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The roster: who is in a game, and what they may do there. Everything here
// runs on the game's goroutine and works from the game's own records.

// addSpectator seats a spectator, unless the player has shut them out.
func (g *Game) addSpectator(info seatInfo) joinResult {
	if g.isBlocked(info.name) && !info.admin {
		return joinResult{refusal: "Spectating this player is restricted."}
	}
	// A restricted player's game is only reachable by typing its URL.
	if g.seats[g.playerSeat].restricted {
		return joinResult{}
	}
	// Whatever is queued predates this spectator; they start from the full
	// state crawl is about to resend.
	g.flush()

	g.lastSeat++
	st := &seat{seatInfo: info, id: g.lastSeat}
	g.seats[st.id] = st
	g.lastJoin = time.Now()
	g.sendClient(st)
	st.send(encodeMessage("watching_started", M{"username": g.username}))
	if g.srv.cfg.MaxChatLength == 0 {
		g.notify(st, "Chat is disabled on this server")
	}
	g.describeSpectators()

	if g.conn != nil {
		// Ask crawl to resend everything. The full map this produces is large,
		// so it is routed only to whoever is marked fresh.
		st.fresh = true
		g.sendToCrawl([]byte(`{"msg":"spectator_joined"}`))
	}
	return joinResult{seatID: st.id, ok: true}
}

// leave removes a spectator who has gone elsewhere.
func (g *Game) leave(seatID int) {
	if st := g.seats[seatID]; st != nil && !st.player {
		delete(g.seats, seatID)
		g.describeSpectators()
	}
}

// eject removes a spectator and sends them to the lobby with a message.
func (g *Game) eject(st *seat, message string) {
	delete(g.seats, st.id)
	st.sess.post(func() { st.sess.seatClosed(g, message, false) })
}

// updateIdentity records that a spectator has logged in.
func (g *Game) updateIdentity(seatID int, info seatInfo) {
	if st := g.seats[seatID]; st != nil {
		st.name, st.admin, st.restricted = info.name, info.admin, info.restricted
		g.describeSpectators()
	}
}

func (g *Game) spectators() []*seat {
	var out []*seat
	for _, st := range g.seats {
		if !st.player {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].name), strings.ToLower(out[j].name)
		return a < b || (a == b && out[i].id < out[j].id)
	})
	return out
}

func (g *Game) spectatorCount() int {
	n := 0
	for _, st := range g.seats {
		if !st.player && !st.chatHidden {
			n++
		}
	}
	return n
}

// spectatorNamed returns the seats of a logged-in spectator, who may be
// watching from more than one window.
func (g *Game) spectatorNamed(name string) []*seat {
	var out []*seat
	for _, st := range g.spectators() {
		if name != "" && st.name == name {
			out = append(out, st)
		}
	}
	return out
}

// describeSpectators tells the roster and the lobby who is watching.
func (g *Game) describeSpectators() {
	urlTemplate := g.srv.cfg.PlayerURL
	wrap := func(name, label, class string) string {
		if urlTemplate == "" {
			return fmt.Sprintf("<span class='%s'>%s</span>", class, label)
		}
		url := strings.ReplaceAll(urlTemplate, "%s", strings.ToLower(name))
		return fmt.Sprintf("<a href='%s' target='_blank' class='%s'>%s</a>", url, class, label)
	}
	var parts []string
	if player := g.seats[g.playerSeat]; player.name != "" && !player.chatHidden {
		parts = append(parts, wrap(player.name, player.name, "player"))
	}
	anon := 0
	for _, st := range g.spectators() {
		switch {
		case st.name == "":
			anon++
		case st.chatHidden:
		case st.admin:
			parts = append(parts, wrap(st.name, st.name+" (admin)", "watcher"))
		default:
			parts = append(parts, wrap(st.name, st.name, "watcher"))
		}
	}
	desc := strings.Join(parts, ", ")
	switch {
	case len(parts) > 0 && anon > 0:
		desc += fmt.Sprintf(" and %d Anon", anon)
	case anon > 0:
		desc = fmt.Sprintf("%d Anon", anon)
	}
	g.broadcast(encodeMessage("update_spectators", M{"count": g.spectatorCount(), "names": desc}))
	g.publish()
}

// isBlocked reports whether name ("" for anonymous) may not watch.
func (g *Game) isBlocked(name string) bool {
	if g.blocked["[all]"] {
		return name != g.username
	}
	if name == "" {
		return g.blocked["[anon]"]
	}
	if k, ok := g.kicked[name]; ok {
		if time.Since(k.start) < k.interval {
			return true
		}
		delete(g.kicked, name)
	}
	return g.blocked[name]
}

// enforceBlocks sends away spectators who are now shut out.
func (g *Game) enforceBlocks() {
	for _, st := range g.spectators() {
		if g.isBlocked(st.name) && !st.admin {
			g.eject(st, "Spectating this player is restricted.")
		}
	}
	g.describeSpectators()
}

// ---- chat ----

// notifyRaw shows HTML in one seat's chat window. Not for text that a player
// or spectator supplied.
func (g *Game) notifyRaw(st *seat, markup string) {
	st.send(encodeMessage("chat", M{"content": "<span class='chat_msg'>" + markup + "</span>", "meta": true}))
}

func (g *Game) notify(st *seat, text string) { g.notifyRaw(st, html.EscapeString(text)) }

// chat handles a line typed into the chat box of a seat.
func (g *Game) chat(seatID int, text string) {
	st := g.seats[seatID]
	switch {
	case st == nil:
	case st.name == "":
		st.send(encodeMessage("chat", M{"content": "You need to log in to send messages!"}))
	case st.restricted:
		st.send(encodeMessage("chat", M{"content": "Your account is restricted; chat unavailable"}))
	case g.chatCommand(st, text):
	case g.srv.cfg.MaxChatLength > 0:
		g.broadcast(encodeMessage("chat", M{"content": "<span class='chat_sender'>" + st.name +
			"</span>: <span class='chat_msg'>" + html.EscapeString(text) + "</span>"}))
		// Crawl records chat in the game's notes.
		g.sendToCrawl([]byte(encodeMessage("note", M{"content": st.name + ": " + text})))
	}
}

func (g *Game) chatHelpLine(st *seat, command, desc string) {
	indent, body := 8, html.EscapeString(desc)
	if command != "" {
		indent, body = 4, html.EscapeString(command)+": "+body
	}
	g.notifyRaw(st, strings.Repeat("&nbsp;", indent)+"<span>"+body+"</span>")
}

func (g *Game) chatHelp(st *seat) {
	// The chat window is close to fixed width; these lines are sized to fit.
	g.notify(st, "The following chat commands are available:")
	g.chatHelpLine(st, "/help", "show chat command help")
	g.chatHelpLine(st, "/hide", "hide the chat window")
	if st.player {
		g.chatHelpLine(st, "/kick <name> [<minutes>]", "kick <name> for some time")
		g.chatHelpLine(st, "", "default: 15 minutes. Not saved across sessions.")
		g.chatHelpLine(st, "/block <name>", "kick and block <name>")
		g.chatHelpLine(st, "", "Must be present in channel. Special names:")
		g.chatHelpLine(st, "", "[anon] disables anonymous spectating")
		g.chatHelpLine(st, "", "[all] disables spectating")
		g.chatHelpLine(st, "/blocklist", "show your entire blocklist")
		g.chatHelpLine(st, "/unblock <name>", "remove <name> from blocklist")
		g.chatHelpLine(st, "/unblock *", "clear your blocklist")
	}
}

// chatCommand runs text if it is a chat command, and reports whether it was.
// Commands that manage spectators are the player's alone.
func (g *Game) chatCommand(st *seat, text string) bool {
	if r := []rune(text); len(r) >= 500 {
		text = string(r[:500])
	}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return false
	}
	command, rest := text, ""
	if i := strings.IndexAny(text, " \t\r\n"); i >= 0 {
		command, rest = text[:i], strings.TrimSpace(text[i:])
	}
	playerOnly := func(verb string, run func()) {
		if st.player {
			run()
		} else {
			g.notify(st, "You do not have permission to "+verb+" spectators.")
		}
	}
	switch strings.ToLower(command) {
	case "/help":
		g.chatHelp(st)
	case "/hide":
		if rest == "forever" {
			st.send(encodeMessage("super_hide_chat", nil))
			st.chatHidden = true
			g.describeSpectators()
		} else {
			st.send(encodeMessage("toggle_chat", nil))
		}
	case "/kick":
		playerOnly("kick", func() { g.kick(st, rest) })
	case "/block", "/mute":
		playerOnly("block", func() { g.block(st, rest) })
	case "/unblock", "/unmute":
		playerOnly("unblock", func() { g.unblock(st, rest) })
	case "/blocklist", "/mutelist":
		if st.player {
			g.showBlocklist(st)
		}
	default:
		return false
	}
	return true
}

func (g *Game) blockedNames() []string {
	names := make([]string, 0, len(g.blocked))
	for n := range g.blocked {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	return names
}

// restoreBlocklist loads the player's saved blocklist when the game starts.
func (g *Game) restoreBlocklist() {
	player := g.seats[g.playerSeat]
	if g.dglLess || player.name == "" {
		return
	}
	for _, name := range strings.Fields(g.srv.db.GetBlocklist(player.name)) {
		if name != player.name {
			g.blocked[name] = true
		}
	}
	if len(g.blocked) > 0 {
		g.notify(player, "Restoring blocklist.")
		g.showBlocklist(player)
	}
}

func (g *Game) saveBlocklist() {
	if player := g.seats[g.playerSeat]; !g.dglLess && player.name != "" {
		g.srv.db.SetBlocklist(player.name, strings.Join(g.blockedNames(), " "))
	}
}

func (g *Game) showBlocklist(st *seat) {
	if names := g.blockedNames(); len(names) == 0 {
		g.notify(st, "No one is blocked.")
	} else {
		g.notify(st, "You have blocked: "+strings.Join(names, ", "))
	}
}

func (g *Game) kick(player *seat, args string) {
	fields := strings.Fields(args)
	target, minutes := "", 15
	if len(fields) > 0 {
		target = fields[0]
	}
	if len(fields) > 1 {
		if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
			minutes = n
		}
	}
	if target == player.name {
		g.notify(player, "You can't kick yourself!")
		return
	}
	seats := g.spectatorNamed(target)
	if len(seats) == 0 {
		g.notify(player, "Kick who??")
		return
	}
	g.logf("Player '%s' has kicked '%s' (%dm)", player.name, target, minutes)
	g.notify(player, fmt.Sprintf("Spectator '%s' has been kicked for %d minutes.", target, minutes))
	g.kicked[target] = kick{start: time.Now(), interval: time.Duration(minutes) * time.Minute}
	for _, st := range seats {
		if !st.admin {
			g.eject(st, "You have been kicked.")
		}
	}
	g.describeSpectators()
}

func (g *Game) block(player *seat, target string) {
	switch {
	case target == player.name:
		g.notify(player, "You can't block yourself!")
		return
	case len(g.spectatorNamed(target)) > 0:
		g.logf("Player '%s' has blocked '%s'", player.name, target)
		g.notify(player, fmt.Sprintf("Spectator '%s' has now been blocked.", target))
	case target == "[anon]":
		g.logf("Player '%s' has disabled anonymous spectating.", player.name)
		g.notify(player, "Anonymous spectating disabled.")
	case target == "[all]":
		g.logf("Player '%s' has disabled spectating.", player.name)
		g.notify(player, "Spectating disabled.")
	default:
		// Only someone present in the channel can be blocked by name.
		g.notify(player, "Block who??")
		return
	}
	g.blocked[target] = true
	g.saveBlocklist()
	g.enforceBlocks()
}

func (g *Game) unblock(player *seat, target string) {
	if target == player.name {
		g.notify(player, "You can't unblock (or block) yourself!")
		return
	}
	if target == "*" {
		if len(g.kicked) > 0 {
			g.kicked = map[string]kick{}
			g.notify(player, "Kicks cleared.")
		}
		if len(g.blocked) == 0 {
			g.notify(player, "No one is blocked!")
			return
		}
		g.logf("Player '%s' has cleared their blocklist.", player.name)
		g.notify(player, "You have cleared your blocklist.")
		g.blocked = map[string]bool{}
		g.saveBlocklist()
		return
	}

	unkicked := false
	if _, ok := g.kicked[target]; ok {
		delete(g.kicked, target)
		g.notify(player, fmt.Sprintf("Removing '%s' from kicks", target))
		unkicked = true
	}
	if !g.blocked[target] {
		if !unkicked {
			g.notify(player, "Unblock who??")
		}
		return
	}
	g.logf("Player '%s' has unblocked '%s'", player.name, target)
	g.notify(player, fmt.Sprintf("You have unblocked '%s'.", target))
	delete(g.blocked, target)
	g.saveBlocklist()
}
