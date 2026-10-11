// Command geryon is a webtiles server for Dungeon Crawl Stone Soup.
//
// Run it from crawl-ref/source, like the Python server it replaces: the paths
// in the configuration (./rcs, ./crawl, ./webserver/...) are relative to that
// directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	assets "github.com/smw/geryon/client"
	"github.com/smw/geryon/internal/config"
	"github.com/smw/geryon/internal/server"
	"github.com/smw/geryon/internal/userdb"
)

// version is set by release builds.
var version = "dev"

func main() {
	serverPath := flag.String("server-path", "./webserver",
		"directory holding config.yml and games.d/ (both optional)")
	port := flag.Int("port", 0, "port to listen on, overriding bind_port")
	address := flag.String("address", "", "address to listen on, overriding bind_address")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		client, _ := assets.FS.ReadFile("version.txt")
		fmt.Printf("geryon %s (built-in client from crawl %s)\n", version, strings.TrimSpace(string(client)))
		return
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	cfg, err := config.Load(*serverPath)
	if err != nil {
		log.Fatalf("Configuration error: %v", err)
	}
	if *port != 0 {
		cfg.BindPort = *port
	}
	if *address != "" {
		cfg.BindAddress = *address
	}

	var games *config.GameSet
	if cfg.DGLMode {
		builtinGames, _ := fs.Sub(assets.FS, "games.d")
		if games, err = config.LoadGames(cfg, builtinGames); err != nil {
			log.Fatalf("Game configuration error: %v", err)
		}
	} else {
		games = &config.GameSet{}
	}

	db, err := userdb.Open(cfg)
	if err != nil {
		log.Fatalf("Database error: %v", err)
	}
	defer db.Close()

	srv, err := server.New(cfg, games, db)
	if err != nil {
		log.Fatalf("Startup error: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Printf("Server error: %v", err)
		os.Exit(1)
	}
}
