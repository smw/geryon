package server

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/smw/geryon/internal/tmpl"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", s.handleIndex)
	mux.HandleFunc("/socket", s.handleSocket)
	mux.HandleFunc("/static/", s.handleStatic)
	mux.HandleFunc("/gamedata/", s.handleGameData)
	mux.HandleFunc("/status/lobby/", s.handleStatusLobby)
	mux.HandleFunc("/status/version/", s.handleStatusVersion)
	return mux
}

// assetFS serves files with a content hash, which is both the ETag and the
// ?v= value that static_url puts in page links.
type assetFS struct {
	fsys   fs.FS
	mu     sync.Mutex
	hashes map[string]hashEntry
}

type hashEntry struct {
	hash    string
	modTime time.Time
	size    int64
}

func newAssetFS(fsys fs.FS) *assetFS {
	return &assetFS{fsys: fsys, hashes: map[string]hashEntry{}}
}

// hash returns the hex MD5 of the named file, or "" if it cannot be read.
// Results are cached until the file's size or modification time changes.
func (a *assetFS) hash(name string) string {
	info, err := fs.Stat(a.fsys, name)
	if err != nil || info.IsDir() {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.hashes[name]; ok && e.modTime.Equal(info.ModTime()) && e.size == info.Size() {
		return e.hash
	}
	data, err := fs.ReadFile(a.fsys, name)
	if err != nil {
		return ""
	}
	sum := md5.Sum(data)
	e := hashEntry{hash: hex.EncodeToString(sum[:]), modTime: info.ModTime(), size: info.Size()}
	a.hashes[name] = e
	return e.hash
}

var extraTypes = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".ttf":   "font/ttf",
	".otf":   "font/otf",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// serve writes the named file. noCache forbids caching outright; otherwise a
// request carrying ?v= (a versioned link) may be cached indefinitely, and
// anything else is revalidated against its ETag.
func (a *assetFS) serve(w http.ResponseWriter, r *http.Request, name string, noCache bool) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	hash := a.hash(name)
	if hash == "" {
		http.NotFound(w, r)
		return
	}
	f, err := a.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	content, ok := f.(io.ReadSeeker)
	if !ok {
		data, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		content = bytes.NewReader(data)
	}

	ext := path.Ext(name)
	ctype := extraTypes[ext]
	if ctype == "" {
		ctype = mime.TypeByExtension(ext)
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.Header().Set("ETag", `"`+hash+`"`)
	switch {
	case noCache:
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
	case r.URL.Query().Has("v"):
		w.Header().Set("Cache-Control", "max-age=315360000")
	}
	http.ServeContent(w, r, name, time.Time{}, content)
}

func (s *Server) staticURL(name string) string {
	url := "/static/" + name
	if h := s.static.hash(name); h != "" {
		url += "?v=" + h
	}
	return url
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	s.static.serve(w, r, strings.TrimPrefix(r.URL.Path, "/static/"), s.cfg.NoCache)
}

func (s *Server) handleGameData(w http.ResponseWriter, r *http.Request) {
	version, name, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/gamedata/"), "/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.clientMu.RLock()
	files := s.clientAssets[version]
	s.clientMu.RUnlock()
	if files == nil {
		http.NotFound(w, r)
		return
	}
	files.serve(w, r, name, s.cfg.GameDataNoCache)
}

// renderTemplate renders one of the server's HTML templates.
func (s *Server) renderTemplate(name string, values map[string]any) (string, error) {
	src, err := fs.ReadFile(s.templates, name)
	if err != nil {
		return "", err
	}
	return tmpl.Render(string(src), &tmpl.Env{
		Values: values,
		// Password reset by email is not implemented in this server.
		Config:    map[string]any{"allow_password_reset": false, "admin_password_reset": false},
		StaticURL: s.staticURL,
		Load: func(name string) (string, error) {
			data, err := fs.ReadFile(s.templates, name)
			return string(data), err
		},
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	protocol := "ws://"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		protocol = "wss://"
	}
	page, err := s.renderTemplate("client.html", map[string]any{
		"socket_server": protocol + r.Host + "/socket",
		"game_version":  s.version,
	})
	if err != nil {
		log.Printf("Error rendering client.html: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	io.WriteString(w, page)
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 16384,
	// The Python server accepts any origin; a session is authenticated by its
	// own login messages, not by cookies sent with the upgrade.
	CheckOrigin:  func(*http.Request) bool { return true },
	Subprotocols: []string{"no-compression"},
}

func (s *Server) handleSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	s.newSession(ws, ip)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) handleStatusLobby(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.hub.statusLobby())
}

func (s *Server) handleStatusVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"webtiles": s.version, "server": "go"})
}
