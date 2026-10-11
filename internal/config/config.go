// Package config loads the webtiles server configuration (config.yml) and the
// game definitions (games.d/*.yaml).
package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds the server options. Field defaults match the Python server's
// stock config.py; values in config.yml override them.
type Config struct {
	// Directory holding config.yml, games.d/ and webtiles/version.txt.
	ServerPath string `yaml:"-"`

	BindAddress string `yaml:"bind_address"`
	BindPort    int    `yaml:"bind_port"`

	PasswordDB string `yaml:"password_db"`
	SettingsDB string `yaml:"settings_db"`

	// The version-independent part of crawl's browser client.
	StaticPath   string `yaml:"static_path"`
	TemplatePath string `yaml:"template_path"`

	ServerID          string `yaml:"server_id"`
	ServerSocketPath  string `yaml:"server_socket_path"`
	InitPlayerProgram string `yaml:"init_player_program"`
	PlayerURL         string `yaml:"player_url"`
	Autologin         string `yaml:"autologin"`

	DGLMode           bool `yaml:"dgl_mode"`
	GameDataNoCache   bool `yaml:"game_data_no_cache"`
	NoCache           bool `yaml:"no_cache"`
	AllowAnonSpectate bool `yaml:"allow_anon_spectate"`
	EnableTtyrecs     bool `yaml:"enable_ttyrecs"`

	NewAccountsDisabled bool `yaml:"new_accounts_disabled"`
	NewAccountsHold     bool `yaml:"new_accounts_hold"`

	MaxConnections     int     `yaml:"max_connections"`
	ConnectionTimeout  float64 `yaml:"connection_timeout"`   // seconds
	MaxIdleTime        float64 `yaml:"max_idle_time"`        // seconds
	MaxLobbyIdleTime   float64 `yaml:"max_lobby_idle_time"`  // seconds
	KillTimeout        float64 `yaml:"kill_timeout"`         // seconds
	LobbyUpdateRate    float64 `yaml:"lobby_update_rate"`    // seconds
	LoginTokenLifetime float64 `yaml:"login_token_lifetime"` // days
	MaxChatLength      int     `yaml:"max_chat_length"`
	RecordingTermSize  [2]int  `yaml:"recording_term_size"` // columns, lines

	NickRegex       string `yaml:"nick_regex"`
	MaxPasswdLength int    `yaml:"max_passwd_length"`
	CryptAlgorithm  string `yaml:"crypt_algorithm"`
	CryptSaltLength int    `yaml:"crypt_salt_length"`

	UseGameYAML *bool     `yaml:"use_game_yaml"`
	Games       yaml.Node `yaml:"games"`
	Templates   yaml.Node `yaml:"templates"`
}

// Defaults returns the configuration used when config.yml sets nothing.
func Defaults() *Config {
	return &Config{
		BindAddress:        "",
		BindPort:           8080,
		PasswordDB:         "./webserver/passwd.db3",
		StaticPath:         "./webserver/static",
		TemplatePath:       "./webserver/templates",
		InitPlayerProgram:  "./util/webtiles-init-player.sh",
		DGLMode:            true,
		GameDataNoCache:    true,
		AllowAnonSpectate:  true,
		EnableTtyrecs:      true,
		MaxConnections:     100,
		ConnectionTimeout:  10 * 60,
		MaxIdleTime:        5 * 60 * 60,
		MaxLobbyIdleTime:   3 * 60 * 60,
		KillTimeout:        10,
		LobbyUpdateRate:    2,
		LoginTokenLifetime: 7,
		MaxChatLength:      1000,
		RecordingTermSize:  [2]int{80, 24},
		NickRegex:          `^[a-zA-Z0-9]{3,20}$`,
		MaxPasswdLength:    20,
		CryptAlgorithm:     "broken",
		CryptSaltLength:    16,
	}
}

// Load reads serverPath/config.yml over the defaults. A missing file is fine.
func Load(serverPath string) (*Config, error) {
	c := Defaults()
	c.ServerPath = serverPath
	path := filepath.Join(serverPath, "config.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		if err := yaml.Unmarshal(data, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		log.Printf("Loading config overrides from: '%s'", path)
		warnUnknownKeys(path, data)
	}
	if c.SettingsDB == "" {
		c.SettingsDB = filepath.Join(filepath.Dir(c.PasswordDB), "user_settings.db3")
	}
	return c, nil
}

// Options the Python server accepts that this server does not act on.
func warnUnknownKeys(path string, data []byte) {
	var raw map[string]any
	if yaml.Unmarshal(data, &raw) != nil {
		return
	}
	known := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		known[strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]] = true
	}
	for k := range raw {
		if !known[k] {
			log.Printf("Warning: option '%s' in %s is not supported by this server; ignoring it", k, path)
		}
	}
}
