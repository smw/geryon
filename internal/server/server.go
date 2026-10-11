// Package server is the webtiles server: it serves the browser client, runs
// crawl processes for players and relays messages between the two.
//
// There are three kinds of moving part, each on its own goroutine:
//
//   - A Session per browser connection handles that browser's requests.
//   - A Game per running crawl process relays between crawl and the browsers
//     seated at it, and owns everything about that game.
//   - The Hub is the little that is shared: the list of games the lobby
//     shows, who is connected, login tokens. It is a locked structure.
//
// Waiting only ever goes one way - a session may wait on a game or the hub,
// a game may wait on the hub - so nothing can deadlock, and a game or a
// browser that is slow or stuck delays only itself. Output to browsers goes
// through each connection's Outbox, which never blocks the sender.
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	assets "github.com/smw/geryon/client"
	"github.com/smw/geryon/internal/config"
	"github.com/smw/geryon/internal/userdb"
)

type Server struct {
	cfg     *config.Config
	games   *config.GameSet
	db      *userdb.DB
	hub     *Hub
	version string // crawl version the server was built alongside
	sockDir string // where the server's ends of the game sockets live

	static    *assetFS
	templates fs.FS

	clientMu     sync.RWMutex
	clientAssets map[string]*assetFS // game client version hash -> its static files
}

// New prepares a server; Run starts it.
func New(cfg *config.Config, games *config.GameSet, db *userdb.DB) (*Server, error) {
	s := &Server{
		cfg:          cfg,
		games:        games,
		db:           db,
		hub:          newHub(),
		version:      loadVersion(cfg),
		clientAssets: map[string]*assetFS{},
	}

	staticFS, err := fs.Sub(assets.FS, "static")
	if err != nil {
		return nil, err
	}
	if cfg.StaticPath != "" {
		staticFS = os.DirFS(cfg.StaticPath)
	}
	s.static = newAssetFS(staticFS)

	if s.templates, err = fs.Sub(assets.FS, "templates"); err != nil {
		return nil, err
	}
	if cfg.TemplatePath != "" {
		s.templates = os.DirFS(cfg.TemplatePath)
	}

	s.sockDir, err = os.MkdirTemp(cfg.ServerSocketPath, "webtiles-")
	if err != nil {
		return nil, err
	}
	return s, nil
}

// loadVersion returns the crawl version to tell browsers, which they use to
// key their caches: the one of the crawl checkout the server runs in if there
// is one, otherwise the one the built-in client came from.
func loadVersion(cfg *config.Config) string {
	data, err := os.ReadFile(filepath.Join(cfg.ServerPath, "webtiles", "version.txt"))
	if err != nil {
		data, err = assets.FS.ReadFile("version.txt")
	}
	if err != nil {
		return "unknown"
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return strings.TrimSpace(line)
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// registerClient makes a game client's static files available under
// /gamedata/<version>/.
func (s *Server) registerClient(version string, static fs.FS) {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.clientAssets[version] == nil {
		s.clientAssets[version] = newAssetFS(static)
	}
}

// Run serves until ctx is cancelled, then shuts games down cleanly: every
// player's game is asked to save and exit before Run returns.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.BindAddress, fmt.Sprint(s.cfg.BindPort)))
	if err != nil {
		return err
	}
	defer os.RemoveAll(s.sockDir)

	// Housekeeping: lobby updates go out in batches, and old tokens expire.
	stopHousekeeping := make(chan struct{})
	defer close(stopHousekeeping)
	go func() {
		lobby := time.NewTicker(seconds(max(s.cfg.LobbyUpdateRate, 0.1)))
		defer lobby.Stop()
		tokens := time.NewTicker(time.Hour)
		defer tokens.Stop()
		for {
			select {
			case <-lobby.C:
				s.hub.flushLobby()
			case <-tokens.C:
				s.hub.purgeTokens()
			case <-stopHousekeeping:
				return
			}
		}
	}()

	httpServer := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 30 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(ln) }()
	log.Printf("DCSS Webtiles server started on %s (PID: %d, crawl %s)", ln.Addr(), os.Getpid(), s.version)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	log.Printf("Shutting down server...")
	// Stop accepting connections, then stop the games.
	ln.Close()
	for _, sess := range s.hub.shutdown() {
		sess.post(sess.shutdown)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		sessions, games := s.hub.counts()
		if sessions == 0 && games == 0 {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("Stop failed after 30s. Remaining: %s", s.hub.describe())
			break
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		httpServer.Close()
	}
	log.Printf("Bye!")
	return nil
}
