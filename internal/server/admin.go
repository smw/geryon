package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/smw/geryon/internal/userdb"
)

// The admin pages and the metrics endpoint. They are served on their own
// listener (admin_address), never on the public one, and have no login: the
// listener is meant to be reachable only by whoever runs the server.
//
// Two things guard against a web page in the administrator's browser driving
// them: requests must name the server by IP address, localhost or a configured
// host (which defeats DNS rebinding), and every action carries a token that
// only the admin page itself contains (which defeats cross-site form posts).

func (s *Server) adminRoutes() http.Handler {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		panic(err)
	}
	a := &admin{srv: s, token: hex.EncodeToString(token)}

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	mux.Handle("GET /admin/{$}", a.guard(a.page))
	for action, handler := range map[string]func(r *http.Request) (string, error){
		"stop-game":    a.stopGame,
		"disconnect":   a.disconnect,
		"announce":     a.announce,
		"reload-games": a.reloadGames,
		"create-user":  a.createUser,
		"set-password": a.setPassword,
		"set-status":   a.setStatus,
	} {
		mux.Handle("POST /admin/"+action, a.guard(a.action(handler)))
	}
	return mux
}

type admin struct {
	srv   *Server
	token string
}

// hostAllowed reports whether a request's Host header names this server in a
// way no outside web page can arrange.
func (a *admin) hostAllowed(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" || net.ParseIP(host) != nil {
		return true
	}
	for _, allowed := range a.srv.cfg.AdminAllowedHosts {
		if strings.EqualFold(host, allowed) {
			return true
		}
	}
	return false
}

func (a *admin) guard(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hostAllowed(r.Host) {
			http.Error(w, "This host name is not allowed to reach the admin pages; see admin_allowed_hosts.",
				http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

// action wraps a form handler: check the token, run it, and go back to the
// page with what happened.
func (a *admin) action(run func(r *http.Request) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.PostFormValue("token")), []byte(a.token)) != 1 {
			http.Error(w, "Missing or stale form token; reload the admin page and try again.", http.StatusForbidden)
			return
		}
		done, err := run(r)
		q := url.Values{}
		if err != nil {
			log.Printf("Admin: %s failed: %v", r.URL.Path, err)
			q.Set("error", err.Error())
		} else {
			log.Printf("Admin: %s", done)
			q.Set("done", done)
		}
		http.Redirect(w, r, "/admin/?"+q.Encode(), http.StatusSeeOther)
	}
}

// ---- actions ----

func (a *admin) stopGame(r *http.Request) (string, error) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	g := a.srv.hub.gameByID(id)
	if g == nil {
		return "", fmt.Errorf("game %d is not running", id)
	}
	g.Stop()
	return fmt.Sprintf("asked %s's game to save and exit", g.username), nil
}

func (a *admin) disconnect(r *http.Request) (string, error) {
	id, _ := strconv.Atoi(r.PostFormValue("id"))
	sess := a.srv.hub.sessionByID(id)
	if sess == nil {
		return "", fmt.Errorf("connection %d is gone", id)
	}
	sess.post(func() { sess.kick("You were disconnected by an administrator.") })
	return fmt.Sprintf("disconnected connection %d", id), nil
}

func (a *admin) announce(r *http.Request) (string, error) {
	text := strings.TrimSpace(r.PostFormValue("text"))
	if text == "" {
		return "", fmt.Errorf("the announcement is empty")
	}
	a.srv.announce(text)
	return "announced: " + text, nil
}

func (a *admin) reloadGames(r *http.Request) (string, error) {
	if err := a.srv.ReloadGames(); err != nil {
		return "", fmt.Errorf("game definitions not reloaded, the previous ones stay in force: %w", err)
	}
	return "reloaded the game definitions", nil
}

func (a *admin) createUser(r *http.Request) (string, error) {
	name := strings.TrimSpace(r.PostFormValue("username"))
	if msg := a.srv.db.Create(name, r.PostFormValue("password"), r.PostFormValue("email")); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	return "created user " + name, nil
}

func (a *admin) setPassword(r *http.Request) (string, error) {
	name := r.PostFormValue("username")
	if msg := a.srv.db.SetPassword(name, r.PostFormValue("password")); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	return "set a new password for " + name, nil
}

func (a *admin) setStatus(r *http.Request) (string, error) {
	name, status, on := r.PostFormValue("username"), r.PostFormValue("status"), r.PostFormValue("on") == "1"
	if err := a.srv.db.SetStatus(name, status, on); err != nil {
		return "", err
	}
	// Make it take effect now for anyone connected as that user: a session
	// rereads its account, and a banned one is logged out and its game saved.
	for _, sess := range a.srv.hub.sessionsOf(name) {
		sess.post(func() { sess.refreshUser() })
	}
	verb := map[bool]string{true: "set", false: "cleared"}[on]
	return fmt.Sprintf("%s %s for %s", verb, status, name), nil
}

// ---- the page ----

type adminUser struct {
	userdb.UserInfo
	Admin, Banned, Hold bool
	Online              int
}

type adminGameDef struct {
	ID, Name, Binary string
	Missing          bool
}

func (a *admin) page(w http.ResponseWriter, r *http.Request) {
	s := a.srv
	sessions := s.hub.sessionViews()
	online := map[string]int{}
	for _, v := range sessions {
		if v.User != "" {
			online[strings.ToLower(v.User)]++
		}
	}

	var users []adminUser
	var userErr string
	if s.cfg.DGLMode {
		list, err := s.db.List()
		if err != nil {
			userErr = err.Error()
		}
		for _, u := range list {
			users = append(users, adminUser{UserInfo: u,
				Admin:  userdb.IsAdmin(u.Flags),
				Banned: userdb.IsBanned(u.Flags),
				Hold:   userdb.AccountHold(u.Flags),
				Online: online[strings.ToLower(u.Username)]})
		}
	}

	var defs []adminGameDef
	for _, g := range s.gameSet().Games {
		binary := g.Str("crawl_binary", "")
		_, err := os.Stat(binary)
		defs = append(defs, adminGameDef{ID: g.ID, Name: g.Str("name", ""), Binary: binary, Missing: err != nil})
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	data := map[string]any{
		"Token":      a.token,
		"Done":       r.URL.Query().Get("done"),
		"Error":      r.URL.Query().Get("error"),
		"Version":    BuildVersion,
		"Crawl":      s.version(),
		"Uptime":     time.Since(s.started).Round(time.Second).String(),
		"Goroutines": runtime.NumGoroutine(),
		"MemoryMB":   fmt.Sprintf("%.1f", float64(mem.Sys)/(1<<20)),
		"Public":     net.JoinHostPort(s.cfg.BindAddress, strconv.Itoa(s.cfg.BindPort)),
		"Games":      s.hub.gameViews(),
		"Sessions":   sessions,
		"Users":      users,
		"UserError":  userErr,
		"Accounts":   s.cfg.DGLMode,
		"GameDefs":   defs,
		"Now":        time.Now(),
	}
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := adminTemplate.Execute(w, data); err != nil {
		log.Printf("Admin page: %v", err)
	}
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// dict builds the argument of a sub-template from key, value pairs.
func dict(pairs ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[fmt.Sprint(pairs[i])] = pairs[i+1]
	}
	return m
}

var adminTemplate = template.Must(template.New("admin").Funcs(template.FuncMap{"ago": ago, "dict": dict}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>geryon admin</title>
<style>
  :root { color-scheme: light dark; --line: #8884; --dim: #888; --bad: #c33; --good: #2a7; }
  body { font: 14px/1.45 system-ui, sans-serif; margin: 0 auto; padding: 16px; max-width: 1100px; }
  h1 { font-size: 20px; margin: 0 0 4px; }
  h2 { font-size: 15px; margin: 28px 0 8px; border-bottom: 1px solid var(--line); padding-bottom: 4px; }
  .dim { color: var(--dim); }
  .note { padding: 8px 12px; border-radius: 6px; margin: 12px 0; border: 1px solid var(--good); }
  .note.error { border-color: var(--bad); }
  .scroll { overflow-x: auto; }
  table { border-collapse: collapse; width: 100%; }
  th, td { text-align: left; padding: 5px 10px 5px 0; border-bottom: 1px solid var(--line); white-space: nowrap; vertical-align: middle; }
  th { font-weight: 600; color: var(--dim); font-size: 12px; }
  td.wrap { white-space: normal; min-width: 14em; }
  form { display: inline; margin: 0; }
  form.block { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin: 8px 0; }
  input[type=text], input[type=password] { font: inherit; padding: 4px 6px; min-width: 0; }
  input.wide { flex: 1 1 260px; }
  button { font: inherit; padding: 3px 9px; cursor: pointer; }
  .tag { font-size: 11px; padding: 1px 6px; border-radius: 9px; border: 1px solid var(--line); }
  .tag.bad { border-color: var(--bad); color: var(--bad); }
  .stats { display: flex; flex-wrap: wrap; gap: 6px 24px; margin: 8px 0; }
  .stats b { font-weight: 600; }
  code { font-size: 12px; }
</style>
</head>
<body>
<h1>geryon admin</h1>
<div class="stats dim">
  <span>geryon <b>{{.Version}}</b></span>
  <span>crawl client <b>{{.Crawl}}</b></span>
  <span>up <b>{{.Uptime}}</b></span>
  <span>public listener <b>{{.Public}}</b></span>
  <span>{{.Goroutines}} goroutines, {{.MemoryMB}} MB</span>
  <span><a href="/metrics">metrics</a></span>
  <span><a href="/admin/">refresh</a></span>
</div>
{{if .Done}}<div class="note">Done: {{.Done}}</div>{{end}}
{{if .Error}}<div class="note error">Failed: {{.Error}}</div>{{end}}

<h2>Games in progress ({{len .Games}})</h2>
{{if .Games}}<div class="scroll"><table>
<tr><th>Player</th><th>Game</th><th>Where</th><th>Watching</th><th>Idle</th><th>Running</th><th>PID</th><th>Binary</th><th></th></tr>
{{range .Games}}<tr>
  <td>{{.Player}}</td><td>{{.GameID}}</td>
  <td class="wrap">{{.Where}}{{if .Milestone}} <span class="dim">({{.Milestone}})</span>{{end}}</td>
  <td>{{.Spectators}}</td>
  <td>{{if gt .Idle 30}}{{.Idle}}s{{else}}<span class="dim">active</span>{{end}}</td>
  <td>{{ago $.Now .Started}}</td><td>{{.PID}}</td><td><code>{{.Binary}}</code></td>
  <td><form method="post" action="/admin/stop-game"><input type="hidden" name="token" value="{{$.Token}}">
      <input type="hidden" name="id" value="{{.ID}}"><button title="Crawl saves the game and exits; the player returns to the lobby">Save &amp; stop</button></form></td>
</tr>{{end}}
</table></div>{{else}}<p class="dim">Nobody is playing.</p>{{end}}

<h2>Connections ({{len .Sessions}})</h2>
{{if .Sessions}}<div class="scroll"><table>
<tr><th>#</th><th>User</th><th>Doing</th><th>From</th><th>Connected</th><th></th></tr>
{{range .Sessions}}<tr>
  <td>{{.ID}}</td>
  <td>{{if .User}}{{.User}}{{if .Admin}} <span class="tag">admin</span>{{end}}{{else}}<span class="dim">anonymous</span>{{end}}</td>
  <td>{{if eq .State "playing"}}playing {{.Target}}{{else if eq .State "watching"}}watching {{.Target}}{{else if .State}}{{.State}}{{else}}<span class="dim">between places</span>{{end}}</td>
  <td>{{.IP}}</td><td>{{ago $.Now .Connected}}</td>
  <td><form method="post" action="/admin/disconnect"><input type="hidden" name="token" value="{{$.Token}}">
      <input type="hidden" name="id" value="{{.ID}}"><button title="A game being played is saved first">Disconnect</button></form></td>
</tr>{{end}}
</table></div>{{else}}<p class="dim">Nobody is connected.</p>{{end}}

<h2>Announcement</h2>
<form class="block" method="post" action="/admin/announce"><input type="hidden" name="token" value="{{.Token}}">
  <input class="wide" type="text" name="text" placeholder="Shown to everyone playing or watching" required>
  <button>Announce</button>
</form>

{{if .Accounts}}
<h2>Accounts ({{len .Users}})</h2>
{{if .UserError}}<div class="note error">{{.UserError}}</div>{{end}}
<div class="scroll"><table>
<tr><th>User</th><th>Email</th><th>Status</th><th>Change status</th><th>New password</th></tr>
{{range .Users}}<tr>
  <td>{{.Username}}{{if .Online}} <span class="tag">online</span>{{end}}</td>
  <td>{{if .Email}}{{.Email}}{{else}}<span class="dim">-</span>{{end}}</td>
  <td>{{if .Admin}}<span class="tag">admin</span> {{end}}{{if .Banned}}<span class="tag bad">banned</span> {{end}}{{if .Hold}}<span class="tag bad">on hold</span>{{end}}</td>
  <td>
    {{template "status" dict "T" $.Token "U" .Username "S" "admin" "On" .Admin "Set" "Make admin" "Clear" "Remove admin"}}
    {{template "status" dict "T" $.Token "U" .Username "S" "ban" "On" .Banned "Set" "Ban" "Clear" "Unban"}}
    {{template "status" dict "T" $.Token "U" .Username "S" "hold" "On" .Hold "Set" "Hold" "Clear" "Release hold"}}
  </td>
  <td><form method="post" action="/admin/set-password"><input type="hidden" name="token" value="{{$.Token}}">
      <input type="hidden" name="username" value="{{.Username}}">
      <input type="password" name="password" size="12" autocomplete="new-password" required> <button>Set</button></form></td>
</tr>{{end}}
</table></div>
<p class="dim">A banned account cannot log in. An account on hold can play, but is hidden from the lobby and cannot chat or spectate.</p>
<form class="block" method="post" action="/admin/create-user"><input type="hidden" name="token" value="{{.Token}}">
  <input type="text" name="username" placeholder="New user name" required>
  <input type="password" name="password" placeholder="Password" autocomplete="new-password" required>
  <input type="text" name="email" placeholder="Email (optional)">
  <button>Create account</button>
  <span class="dim">Works even when public registration is off.</span>
</form>
{{end}}

<h2>Game definitions ({{len .GameDefs}})</h2>
<div class="scroll"><table>
<tr><th>Id</th><th>Name</th><th>Binary</th></tr>
{{range .GameDefs}}<tr><td>{{.ID}}</td><td>{{.Name}}</td>
  <td><code>{{.Binary}}</code>{{if .Missing}} <span class="tag bad">missing</span>{{end}}</td></tr>{{end}}
</table></div>
<form class="block" method="post" action="/admin/reload-games"><input type="hidden" name="token" value="{{.Token}}">
  <button>Reload from games.d</button>
  <span class="dim">Games in progress keep the definition they started with.</span>
</form>
</body>
</html>
{{define "status"}}<form method="post" action="/admin/set-status"><input type="hidden" name="token" value="{{.T}}">
<input type="hidden" name="username" value="{{.U}}"><input type="hidden" name="status" value="{{.S}}">
{{if .On}}<input type="hidden" name="on" value="0"><button>{{.Clear}}</button>{{else}}<input type="hidden" name="on" value="1"><button>{{.Set}}</button>{{end}}</form>{{end}}
`))
