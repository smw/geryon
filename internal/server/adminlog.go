package server

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
)

// The log viewer for one game: /admin/game/<id>/log.
//
//	(no format)    an HTML page that follows the log as it grows
//	?format=json   the lines from ?after=<n> on, which is what the page polls
//	?format=text   the whole log as plain text
func (a *admin) gameLog(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	glog, view, ended, found := a.srv.hub.gameLogByID(id)
	if !found {
		http.Error(w, "No such game. Logs are kept for running games and the last "+
			strconv.Itoa(endedGamesKept)+" that ended.", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	switch r.URL.Query().Get("format") {
	case "json":
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		lines, next := glog.since(after)
		type line struct {
			T string `json:"t"`
			K string `json:"k"`
			S string `json:"s"`
		}
		out := struct {
			Lines   []line `json:"lines"`
			Next    int    `json:"next"`
			Skipped int    `json:"skipped"` // lines before the first one returned that are gone
			Running bool   `json:"running"`
			Status  string `json:"status"`
		}{Lines: []line{}, Next: next, Running: ended == nil, Status: gameStatus(view, ended)}
		out.Skipped = max(0, next-len(lines)-after)
		for _, l := range lines {
			out.Lines = append(out.Lines, line{T: l.Time.Format("15:04:05.000"), K: l.Source, S: l.Text})
		}
		writeJSON(w, out)

	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		lines, _ := glog.since(0)
		for _, l := range lines {
			prefix := "       "
			if l.Source == "server" {
				prefix = "geryon "
			}
			fmt.Fprintf(w, "%s %s%s\n", l.Time.Format("2006-01-02 15:04:05.000"), prefix, l.Text)
		}

	default:
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		data := map[string]any{"ID": id, "Game": view, "Status": gameStatus(view, ended), "Limit": gameLogLimit}
		if err := gameLogTemplate.Execute(w, data); err != nil {
			log.Printf("Admin log page: %v", err)
		}
	}
}

// gameStatus is the one-line state of a game shown above its log.
func gameStatus(view gameView, ended *endedGame) string {
	if ended == nil {
		status := "running, crawl PID " + strconv.Itoa(view.PID)
		if view.Where != "" {
			status += ", " + view.Where
		}
		return status
	}
	status := "ended " + ended.Ended.Format("15:04:05")
	switch {
	case ended.Reason == "":
		status += ", never started"
	case ended.Message != "":
		status += ": " + ended.Reason + " (" + ended.Message + ")"
	default:
		status += ": " + ended.Reason
	}
	return status
}

var gameLogTemplate = template.Must(template.New("log").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Game.Player}} · game log</title>
<style>
  :root { color-scheme: light dark; --line: #8884; --dim: #888; }
  html, body { height: 100%; }
  body { font: 14px/1.45 system-ui, sans-serif; margin: 0; display: flex; flex-direction: column; }
  header { padding: 12px 16px 8px; border-bottom: 1px solid var(--line); }
  h1 { font-size: 17px; margin: 0 0 2px; }
  .bar { display: flex; flex-wrap: wrap; gap: 6px 18px; align-items: center; color: var(--dim); }
  .bar label { color: inherit; }
  input[type=text] { font: inherit; padding: 2px 6px; width: 14em; max-width: 100%; }
  #log { flex: 1; overflow: auto; margin: 0; padding: 8px 16px 16px; font: 12px/1.5 ui-monospace, monospace; white-space: pre-wrap; overflow-wrap: anywhere; }
  #log .t { color: var(--dim); user-select: none; }
  #log .server { color: var(--dim); font-style: italic; }
  #log .gap { color: var(--dim); }
</style>
</head>
<body>
<header>
  <h1>{{.Game.Player}} <span style="font-weight:normal">· {{.Game.GameID}} · game {{.ID}}</span></h1>
  <div class="bar">
    <span id="status">{{.Status}}</span>
    <span><span id="count">0</span> lines</span>
    <label><input type="checkbox" id="follow" checked> follow</label>
    <label>filter <input type="text" id="filter" placeholder="text or /regex/"></label>
    <a href="?format=text">plain text</a>
    <a href="/admin/">admin</a>
  </div>
  <div class="bar">Lines in italics are the server's; the rest is what crawl printed to stderr. The last {{.Limit}} lines are kept.</div>
</header>
<pre id="log"></pre>
<script>
(function () {
  var logEl = document.getElementById("log"), follow = document.getElementById("follow");
  var filterEl = document.getElementById("filter"), next = 0, count = 0, match = null;

  function setFilter() {
    var v = filterEl.value, m = /^\/(.*)\/([a-z]*)$/.exec(v);
    try { match = !v ? null : m ? new RegExp(m[1], m[2]) : null; } catch (e) { match = null; }
    var needle = (!v || m) ? null : v.toLowerCase();
    for (var el = logEl.firstChild; el; el = el.nextSibling) {
      var s = el.dataset.s;
      el.hidden = s !== undefined && (match ? !match.test(s) : needle ? s.toLowerCase().indexOf(needle) < 0 : false);
    }
    filterEl.dataset.needle = needle || "";
  }
  filterEl.addEventListener("input", setFilter);

  function add(cls, time, text) {
    var row = document.createElement("div");
    row.className = cls;
    if (time) {
      var t = document.createElement("span");
      t.className = "t";
      t.textContent = time + "  ";
      row.appendChild(t);
      row.dataset.s = text;
      var needle = filterEl.dataset.needle;
      row.hidden = match ? !match.test(text) : needle ? text.toLowerCase().indexOf(needle) < 0 : false;
    }
    row.appendChild(document.createTextNode(text));
    logEl.appendChild(row);
  }

  function poll() {
    fetch("?format=json&after=" + next, { cache: "no-store" })
      .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function (d) {
        if (d.skipped) add("gap", "", "… " + d.skipped + " earlier lines are no longer kept …");
        d.lines.forEach(function (l) { add(l.k, l.t, l.s); });
        count += d.lines.length;
        next = d.next;
        document.getElementById("count").textContent = count;
        document.getElementById("status").textContent = d.status;
        if (d.lines.length && follow.checked) logEl.scrollTop = logEl.scrollHeight;
        // A finished game's log does not grow, but check a little longer for its last lines.
        setTimeout(poll, d.running ? 1500 : 10000);
      })
      .catch(function () {
        document.getElementById("status").textContent = "cannot reach the server; retrying";
        setTimeout(poll, 5000);
      });
  }
  // Scrolling up stops following; scrolling back to the end resumes it.
  logEl.addEventListener("scroll", function () {
    follow.checked = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 40;
  });
  poll();
})();
</script>
</body>
</html>
`))
