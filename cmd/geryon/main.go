// Command geryon is a webtiles server for Dungeon Crawl Stone Soup.
//
// Run it from crawl-ref/source of a crawl checkout built with
// `make WEBTILES=y`, like the Python server it replaces: the paths in the
// configuration (./rcs, ./crawl, ./webserver/...) are relative to that
// directory, and crawl's browser client is served from ./webserver.
//
// SIGHUP or SIGUSR1 rereads the game definitions (games.d) without
// disturbing games in progress. SIGINT or SIGTERM saves every game and exits.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/smw/geryon/internal/config"
	"github.com/smw/geryon/internal/server"
	"github.com/smw/geryon/internal/userdb"
)

// version is set by release builds.
var version = "dev"

func main() {
	serverPath := flag.String("server-path", "./webserver",
		"directory holding config.yml and games.d/")
	port := flag.Int("port", 0, "port to listen on, overriding bind_port")
	address := flag.String("address", "", "address to listen on, overriding bind_address")
	adminAddress := flag.String("admin", "",
		"address for the admin pages and /metrics, overriding admin_address (e.g. 127.0.0.1:8081); keep it private")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("geryon", version)
		return
	}
	server.BuildVersion = version
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
	if *adminAddress != "" {
		cfg.AdminAddress = *adminAddress
	}

	games := &config.GameSet{}
	if cfg.DGLMode {
		if games, err = config.LoadGames(cfg); err != nil {
			log.Fatalf("Game configuration error: %v", err)
		}
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

	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP, syscall.SIGUSR1)
	go func() {
		for range reload {
			log.Printf("Reloading game definitions...")
			if err := srv.ReloadGames(); err != nil {
				log.Printf("Game configuration error, keeping the previous definitions: %v", err)
			}
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		log.Printf("Server error: %v", err)
		os.Exit(1)
	}
}
