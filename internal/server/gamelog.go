package server

import (
	"sync"
	"time"
)

// gameLog keeps the recent log of one game in memory, for the admin pages:
// what the server did with the game, and what crawl printed to stderr (which
// is where crawl reports errors and where Lua scripts print).
type gameLog struct {
	mu    sync.Mutex
	lines []logLine
	first int // sequence number of lines[0]
}

type logLine struct {
	Time   time.Time
	Source string // "server" or "crawl"
	Text   string
}

// A game keeps at most this many lines; older ones are dropped.
const gameLogLimit = 5000

func (l *gameLog) add(source, text string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, logLine{Time: time.Now(), Source: source, Text: text})
	// Trim in batches, so that not every line past the limit copies the lot.
	if over := len(l.lines) - gameLogLimit; over >= gameLogLimit/10 {
		l.lines = append([]logLine(nil), l.lines[over:]...)
		l.first += over
	}
}

// since returns the lines from sequence number seq on, and the sequence
// number to ask for next time. Lines already dropped are skipped.
func (l *gameLog) since(seq int) (lines []logLine, next int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	next = l.first + len(l.lines)
	if seq < l.first {
		seq = l.first
	}
	if seq >= next {
		return nil, next
	}
	return append([]logLine(nil), l.lines[seq-l.first:]...), next
}
