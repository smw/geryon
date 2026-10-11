// Package crypt implements the crypt(3) password hashes found in webtiles user
// databases: SHA-512 ($6$), SHA-256 ($5$) and traditional DES.
package crypt

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"errors"
	"hash"
	"strconv"
	"strings"
)

const saltChars = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Crypt hashes password using the algorithm and salt described by setting,
// which is either a full hash or a salt prefix, as with crypt(3).
func Crypt(password, setting string) (string, error) {
	switch {
	case strings.HasPrefix(setting, "$6$"):
		return shaCrypt(password, setting, true)
	case strings.HasPrefix(setting, "$5$"):
		return shaCrypt(password, setting, false)
	case strings.HasPrefix(setting, "$") || strings.HasPrefix(setting, "*"):
		return "", errors.New("unsupported crypt algorithm")
	default:
		return desCrypt(password, setting)
	}
}

// Verify reports whether password matches the stored hash.
func Verify(password, stored string) bool {
	h, err := Crypt(password, stored)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h), []byte(stored)) == 1
}

// MakeSalt returns n random characters from the crypt salt alphabet.
func MakeSalt(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = saltChars[b[i]&63]
	}
	return string(b)
}

const (
	roundsDefault = 5000
	roundsMin     = 1000
	roundsMax     = 999999999
)

func shaCrypt(password, setting string, is512 bool) (string, error) {
	prefix := setting[:3]
	rest := setting[3:]
	rounds := roundsDefault
	roundsCustom := false
	if strings.HasPrefix(rest, "rounds=") {
		end := strings.IndexByte(rest, '$')
		if end < 0 {
			return "", errors.New("malformed rounds in salt")
		}
		n, err := strconv.Atoi(rest[len("rounds="):end])
		if err != nil {
			return "", errors.New("malformed rounds in salt")
		}
		rounds = min(max(n, roundsMin), roundsMax)
		roundsCustom = true
		rest = rest[end+1:]
	}
	if i := strings.IndexByte(rest, '$'); i >= 0 {
		rest = rest[:i]
	}
	if len(rest) > 16 {
		rest = rest[:16]
	}
	salt := []byte(rest)
	pw := []byte(password)

	newHash := sha256.New
	if is512 {
		newHash = sha512.New
	}
	size := newHash().Size()

	b := newHash()
	b.Write(pw)
	b.Write(salt)
	b.Write(pw)
	bsum := b.Sum(nil)

	a := newHash()
	a.Write(pw)
	a.Write(salt)
	cnt := len(pw)
	for ; cnt > size; cnt -= size {
		a.Write(bsum)
	}
	a.Write(bsum[:cnt])
	for cnt = len(pw); cnt > 0; cnt >>= 1 {
		if cnt&1 != 0 {
			a.Write(bsum)
		} else {
			a.Write(pw)
		}
	}
	alt := a.Sum(nil)

	dp := newHash()
	for range pw {
		dp.Write(pw)
	}
	p := repeatTo(dp.Sum(nil), len(pw))

	ds := newHash()
	for i := 0; i < 16+int(alt[0]); i++ {
		ds.Write(salt)
	}
	s := repeatTo(ds.Sum(nil), len(salt))

	var c hash.Hash
	for i := 0; i < rounds; i++ {
		c = newHash()
		if i&1 != 0 {
			c.Write(p)
		} else {
			c.Write(alt)
		}
		if i%3 != 0 {
			c.Write(s)
		}
		if i%7 != 0 {
			c.Write(p)
		}
		if i&1 != 0 {
			c.Write(alt)
		} else {
			c.Write(p)
		}
		alt = c.Sum(alt[:0])
	}

	var out strings.Builder
	out.WriteString(prefix)
	if roundsCustom {
		out.WriteString("rounds=" + strconv.Itoa(rounds) + "$")
	}
	out.Write(salt)
	out.WriteByte('$')
	if is512 {
		for i, x, y, z := 0, 0, 21, 42; i < 21; i++ {
			b64From24(&out, alt[x], alt[y], alt[z], 4)
			x, y, z = y+1, z+1, x+1
		}
		b64From24(&out, 0, 0, alt[63], 2)
	} else {
		for i, x, y, z := 0, 0, 10, 20; i < 10; i++ {
			b64From24(&out, alt[x], alt[y], alt[z], 4)
			x, y, z = z+1, x+1, y+1
		}
		b64From24(&out, 0, alt[31], alt[30], 3)
	}
	return out.String(), nil
}

func repeatTo(block []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, block[:min(len(block), n-len(out))]...)
	}
	return out
}

func b64From24(out *strings.Builder, b2, b1, b0 byte, n int) {
	w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
	for ; n > 0; n-- {
		out.WriteByte(saltChars[w&63])
		w >>= 6
	}
}
