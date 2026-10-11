// Package userdb reads and writes the webtiles account database (passwd.db3)
// and the per-user settings database (user_settings.db3). Both keep the schema
// the Python server and dgamelaunch use.
package userdb

import (
	"database/sql"
	"errors"
	"log"
	"os"
	"regexp"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/smw/geryon/internal/config"
	"github.com/smw/geryon/internal/crypt"
)

// Account flags, from dgamelaunch.h plus the webtiles additions.
const (
	FlagAdmin       = 1
	FlagLoginLock   = 2
	FlagPasswdLock  = 4
	FlagEmailLock   = 8
	FlagAccountHold = 16
)

func IsAdmin(flags int) bool     { return flags&FlagAdmin != 0 }
func AccountHold(flags int) bool { return flags&FlagAccountHold != 0 }
func IsBanned(flags int) bool    { return flags&FlagLoginLock != 0 && !AccountHold(flags) }

type UserInfo struct {
	ID       int64
	Username string
	Email    string
	Flags    int
}

type DB struct {
	cfg      *config.Config
	users    *sql.DB // nil when dgl_mode is off
	settings *sql.DB
	nickRe   *regexp.Regexp
}

const usersSchema = `
CREATE TABLE dglusers (
    id INTEGER PRIMARY KEY,
    username TEXT,
    email TEXT,
    env TEXT,
    password TEXT,
    flags INTEGER
);`

const userIndexSchema = `
CREATE UNIQUE INDEX index_username
    ON dglusers (username COLLATE NOCASE);`

const recoverySchema = `
CREATE TABLE recovery_tokens (
    token TEXT PRIMARY KEY,
    token_time TEXT,
    user_id INTEGER NOT NULL,
    FOREIGN KEY(user_id) REFERENCES dglusers(id)
);`

const settingsSchema = `
CREATE TABLE mutesettings (
    username TEXT PRIMARY KEY NOT NULL UNIQUE,
    mutelist TEXT DEFAULT ''
);`

func openDB(path string, create ...string) (*sql.DB, error) {
	_, statErr := os.Stat(path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection: all access comes from the server's single event loop.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, err
	}
	if os.IsNotExist(statErr) {
		log.Printf("Database '%s' didn't exist; creating it now.", path)
		for _, stmt := range create {
			if _, err := db.Exec(stmt); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	return db, nil
}

// Open opens both databases, creating them if they do not exist.
func Open(cfg *config.Config) (*DB, error) {
	nickRe, err := regexp.Compile(cfg.NickRegex)
	if err != nil {
		return nil, errors.New("nick_regex: " + err.Error())
	}
	d := &DB{cfg: cfg, nickRe: nickRe}
	if cfg.DGLMode {
		d.users, err = openDB(cfg.PasswordDB, usersSchema, userIndexSchema, recoverySchema)
		if err != nil {
			return nil, err
		}
	}
	d.settings, err = openDB(cfg.SettingsDB, settingsSchema)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() {
	if d.users != nil {
		d.users.Close()
	}
	d.settings.Close()
}

func (d *DB) userQuery(where string, arg any) *UserInfo {
	var u UserInfo
	var email sql.NullString
	var flags sql.NullInt64
	err := d.users.QueryRow("SELECT id, username, email, flags FROM dglusers WHERE "+where, arg).
		Scan(&u.ID, &u.Username, &email, &flags)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("User database error: %v", err)
		}
		return nil
	}
	u.Email, u.Flags = email.String, int(flags.Int64)
	return &u
}

// GetUserInfo looks a user up by name, ignoring case. Returns nil if unknown.
func (d *DB) GetUserInfo(username string) *UserInfo {
	return d.userQuery("username=? COLLATE NOCASE", username)
}

func (d *DB) getUserByID(id int64) *UserInfo {
	return d.userQuery("id=?", id)
}

func (d *DB) truncate(passwd string) string {
	if r := []rune(passwd); len(r) > d.cfg.MaxPasswdLength {
		return string(r[:d.cfg.MaxPasswdLength])
	}
	return passwd
}

// PasswdMatch checks a login. It returns whether it succeeded, the username in
// its stored spelling, and a reason to show the user for a failure (may be "").
func (d *DB) PasswdMatch(username, passwd string) (ok bool, realName, reason string) {
	var stored sql.NullString
	var flags sql.NullInt64
	err := d.users.QueryRow(
		"SELECT username, password, flags FROM dglusers WHERE username=? COLLATE NOCASE", username).
		Scan(&realName, &stored, &flags)
	if err != nil {
		return false, "", ""
	}
	// A banned player never passes the password check.
	if IsBanned(int(flags.Int64)) {
		return false, realName, "Account is disabled."
	}
	if crypt.Verify(d.truncate(passwd), stored.String) {
		return true, realName, ""
	}
	return false, "", ""
}

func (d *DB) encrypt(passwd string) (string, error) {
	// The Python server's default ("broken") is DES salted with the password
	// itself. Old hashes of that kind still verify, but new ones are never
	// written: anything that isn't SHA-256 gets SHA-512.
	alg := d.cfg.CryptAlgorithm
	if alg != "5" && alg != "6" {
		alg = "6"
	}
	return crypt.Crypt(d.truncate(passwd), "$"+alg+"$"+crypt.MakeSalt(min(d.cfg.CryptSaltLength, 16))+"$")
}

var emailRe = regexp.MustCompile(`^[^@]+@[^@]+\.[^@]+`)

// validateEmail returns a description of the problem, or "". Blank is valid.
func validateEmail(address string) string {
	switch {
	case address == "":
		return ""
	case strings.Contains(address, " "):
		return "Email address can't contain a space"
	case !strings.Contains(address, "@"):
		return "Expected email address to contain the @ symbol"
	case !emailRe.MatchString(address):
		return "Invalid email address"
	case len(address) >= 80:
		return "Email address can't be more than 80 characters"
	}
	return ""
}

// Register creates an account. It returns an error message for the user, or "".
func (d *DB) Register(username, passwd, email string) string {
	if d.cfg.NewAccountsDisabled {
		return "New account creation is disabled."
	}
	return d.Create(username, passwd, email)
}

// Create makes an account whether or not registration is open to the public.
// It returns an error message, or "".
func (d *DB) Create(username, passwd, email string) string {
	if passwd == "" {
		return "The password can't be empty!"
	}
	if msg := validateEmail(email); msg != "" {
		return msg
	}
	username = strings.TrimSpace(username)
	if !d.nickRe.MatchString(username) {
		return "Account creation failed."
	}
	hash, err := d.encrypt(passwd)
	if err != nil {
		return "Account creation failed."
	}
	if d.GetUserInfo(username) != nil {
		return "User already exists!"
	}
	flags := 0
	if d.cfg.NewAccountsHold {
		flags = FlagLoginLock | FlagEmailLock | FlagPasswdLock | FlagAccountHold
	}
	_, err = d.users.Exec(
		"INSERT INTO dglusers (username, email, password, flags, env) VALUES (?, ?, ?, ?, '')",
		username, email, hash, flags)
	if err != nil {
		log.Printf("User database error: %v", err)
		return "Account creation failed."
	}
	return ""
}

// List returns every account, by name.
func (d *DB) List() ([]UserInfo, error) {
	rows, err := d.users.Query(
		"SELECT id, username, email, flags FROM dglusers ORDER BY username COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserInfo
	for rows.Next() {
		var u UserInfo
		var email sql.NullString
		var flags sql.NullInt64
		if err := rows.Scan(&u.ID, &u.Username, &email, &flags); err != nil {
			return nil, err
		}
		u.Email, u.Flags = email.String, int(flags.Int64)
		out = append(out, u)
	}
	return out, rows.Err()
}

// The states an administrator can put an account in.
const (
	// StatusBanned: cannot log in.
	StatusBanned = "ban"
	// StatusHold: can log in and play, but is hidden from the lobby, cannot
	// chat or spectate, and leaves no bones files.
	StatusHold = "hold"
	// StatusAdmin: sees restricted games and the admin panel.
	StatusAdmin = "admin"
)

const holdFlags = FlagLoginLock | FlagEmailLock | FlagPasswdLock | FlagAccountHold

// SetStatus turns one of the Status states on or off for a user. A ban and a
// hold exclude each other: setting one clears the other.
func (d *DB) SetStatus(username, status string, on bool) error {
	u := d.GetUserInfo(username)
	if u == nil {
		return errors.New("no such user: " + username)
	}
	flags := u.Flags
	switch status {
	case StatusAdmin:
		flags &^= FlagAdmin
		if on {
			flags |= FlagAdmin
		}
	case StatusBanned:
		flags &^= holdFlags
		if on {
			flags |= FlagLoginLock
		}
	case StatusHold:
		flags &^= holdFlags
		if on {
			flags |= holdFlags
		}
	default:
		return errors.New("unknown account status: " + status)
	}
	_, err := d.users.Exec("UPDATE dglusers SET flags=? WHERE id=?", flags, u.ID)
	return err
}

// SetPassword replaces a user's password on an administrator's say-so,
// whatever locks the account has. It returns an error message, or "".
func (d *DB) SetPassword(username, passwd string) string {
	u := d.GetUserInfo(username)
	if u == nil {
		return "No such user."
	}
	return d.changePassword(u, passwd)
}

// ChangePassword returns an error message for the user, or "".
func (d *DB) ChangePassword(userID int64, passwd string) string {
	u := d.getUserByID(userID)
	if u == nil {
		return "Invalid account!"
	}
	if u.Flags&FlagPasswdLock != 0 {
		return "Account has a password lock!"
	}
	return d.changePassword(u, passwd)
}

func (d *DB) changePassword(u *UserInfo, passwd string) string {
	if passwd == "" {
		return "The password can't be empty."
	}
	hash, err := d.encrypt(passwd)
	if err == nil {
		_, err = d.users.Exec("UPDATE dglusers SET password=? WHERE id=?", hash, u.ID)
	}
	if err != nil {
		log.Printf("User database error: %v", err)
		return "Could not change the password."
	}
	// A password change invalidates any outstanding recovery tokens.
	d.users.Exec("DELETE FROM recovery_tokens WHERE user_id=?", u.ID)
	return ""
}

// ChangeEmail returns an error message for the user, or "".
func (d *DB) ChangeEmail(userID int64, email string) string {
	if msg := validateEmail(email); msg != "" {
		return msg
	}
	u := d.getUserByID(userID)
	if u == nil {
		return "Invalid account!"
	}
	if u.Flags&FlagEmailLock != 0 {
		return "Account has an email lock!"
	}
	if _, err := d.users.Exec("UPDATE dglusers SET email=? WHERE id=?", email, u.ID); err != nil {
		log.Printf("User database error: %v", err)
		return "Could not change the email address."
	}
	return ""
}

// GetBlocklist returns the user's saved spectator blocklist as stored: names
// separated by spaces. (The table predates the rename from mute to block.)
func (d *DB) GetBlocklist(username string) string {
	var list sql.NullString
	d.settings.QueryRow(
		"SELECT mutelist FROM mutesettings WHERE username=? COLLATE NOCASE", username).Scan(&list)
	return list.String
}

func (d *DB) SetBlocklist(username, blocklist string) {
	_, err := d.settings.Exec(
		"INSERT OR REPLACE INTO mutesettings (username, mutelist) VALUES (?,?)", username, blocklist)
	if err != nil {
		log.Printf("Settings database error: %v", err)
	}
}
