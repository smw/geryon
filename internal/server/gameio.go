package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// terminal runs a crawl process on a pseudo-terminal and records what it
// prints there as a ttyrec. Its channels are read by the game that owns it.
type terminal struct {
	cmd  *exec.Cmd
	ptmx *os.File
	pid  int

	output chan []string // lines printed to the terminal; a read with no complete line sends nil
	stderr chan string   // lines printed to stderr
	exited chan struct{} // closed once the process is gone and both channels above are closed
	input  chan []byte

	mu     sync.Mutex // guards the ttyrec, written by the terminal reader
	ttyrec *bufio.Writer
	file   *os.File
}

// startTerminal launches call. abandon is closed if the owner stops reading
// the terminal's channels, so the readers don't block forever.
func startTerminal(call []string, env map[string]string, cwd string, size [2]int,
	ttyrecPath string, idHeader []byte, abandon <-chan struct{}) (*terminal, error) {
	t := &terminal{
		output: make(chan []string, 16),
		stderr: make(chan string, 16),
		exited: make(chan struct{}),
		input:  make(chan []byte, 256),
	}
	if ttyrecPath != "" {
		f, err := os.Create(ttyrecPath)
		if err != nil {
			return nil, err
		}
		t.file, t.ttyrec = f, bufio.NewWriter(f)
		t.writeChunk(idHeader)
		t.flush()
	}

	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.closeTtyrec()
		return nil, err
	}
	cols, lines := size[0], size[1]
	cmd := exec.Command(call[0], call[1:]...)
	cmd.Dir = cwd
	cmd.Stderr = errWrite
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, fmt.Sprintf("COLUMNS=%d", cols), fmt.Sprintf("LINES=%d", lines), "TERM=linux")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(lines), Cols: uint16(cols)})
	errWrite.Close()
	if err != nil {
		errRead.Close()
		t.closeTtyrec()
		return nil, err
	}
	t.cmd, t.ptmx, t.pid = cmd, ptmx, cmd.Process.Pid

	ptyDone := make(chan struct{})
	go func() {
		defer close(ptyDone)
		defer close(t.output)
		buf := make([]byte, 4096)
		var pending []byte
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				t.writeChunk(buf[:n])
				pending = append(pending, buf[:n]...)
				var out []string
				for {
					i := bytes.IndexByte(pending, '\n')
					if i < 0 {
						break
					}
					if line := bytes.TrimSuffix(pending[:i], []byte("\r")); len(line) > 0 {
						out = append(out, string(line))
					}
					pending = pending[i+1:]
				}
				select {
				case t.output <- out:
				case <-abandon:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	errDone := make(chan struct{})
	go func() {
		defer close(errDone)
		defer close(t.stderr)
		scanner := bufio.NewScanner(errRead)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			if line := string(bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))); line != "" {
				select {
				case t.stderr <- line:
				case <-abandon:
				}
			}
		}
	}()

	go func() {
		for data := range t.input {
			if _, err := ptmx.Write(data); err != nil {
				return
			}
		}
	}()

	go func() {
		cmd.Wait()
		// Let the readers pick up anything still buffered, but don't wait
		// forever on a descendant that kept the terminal open.
		for _, done := range []chan struct{}{ptyDone, errDone} {
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
		ptmx.Close()
		errRead.Close()
		<-ptyDone
		<-errDone
		t.closeTtyrec()
		close(t.exited)
	}()
	return t, nil
}

// writeChunk appends a ttyrec frame: seconds, microseconds and length as
// little-endian 32-bit integers, then the data.
func (t *terminal) writeChunk(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ttyrec == nil {
		return
	}
	now := time.Now()
	var header [12]byte
	binary.LittleEndian.PutUint32(header[0:], uint32(now.Unix()))
	binary.LittleEndian.PutUint32(header[4:], uint32(now.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(header[8:], uint32(len(data)))
	t.ttyrec.Write(header[:])
	t.ttyrec.Write(data)
}

func (t *terminal) flush() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ttyrec != nil {
		t.ttyrec.Flush()
	}
}

func (t *terminal) closeTtyrec() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ttyrec != nil {
		t.ttyrec.Flush()
		t.file.Close()
		t.ttyrec = nil
	}
}

func (t *terminal) signal(sig syscall.Signal) error { return t.cmd.Process.Signal(sig) }

// writeInput types data into the terminal.
func (t *terminal) writeInput(data []byte) {
	select {
	case t.input <- data:
	default:
	}
}

// crawlConn is the server's end of a crawl process's message socket: a Unix
// datagram socket bound to its own path, so that crawl can reply to it.
type crawlConn struct {
	local     *net.UnixConn
	localPath string

	msgs chan []string // messages from crawl, in order
	gone chan struct{} // closed when crawl's socket has disappeared
	send chan []byte
}

// attachCrawl binds localPath and attaches to the crawl socket at crawlPath,
// which must exist. primary is true for the server that launched the process.
func attachCrawl(crawlPath, localPath string, primary bool, abandon <-chan struct{}) (*crawlConn, error) {
	local, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: localPath, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	// Full map updates can be large; make sure a burst of them fits.
	local.SetReadBuffer(212992)
	c := &crawlConn{
		local:     local,
		localPath: localPath,
		msgs:      make(chan []string, 64),
		gone:      make(chan struct{}),
		send:      make(chan []byte, 512),
	}

	go func() {
		buf := make([]byte, 512*1024)
		var pending []byte
		for {
			n, _, err := local.ReadFromUnix(buf)
			if err != nil {
				return
			}
			// Every message from crawl ends with a newline; one that doesn't
			// yet is a fragment of a message split across datagrams.
			pending = append(pending, buf[:n]...)
			if len(pending) == 0 || pending[len(pending)-1] != '\n' {
				continue
			}
			var msgs []string
			for _, m := range bytes.Split(pending, []byte("\n")) {
				if len(m) > 0 {
					msgs = append(msgs, string(m))
				}
			}
			pending = pending[:0]
			select {
			case c.msgs <- msgs:
			case <-abandon:
				return
			}
		}
	}()

	crawlAddr := &net.UnixAddr{Name: crawlPath, Net: "unixgram"}
	go func() {
		gone := false
		for data := range c.send {
			if gone {
				continue
			}
			_, err := local.WriteToUnix(data, crawlAddr)
			if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, net.ErrClosed) {
				// The game's socket is gone: the process has exited or is about to.
				gone = true
				close(c.gone)
			}
		}
	}()

	c.sendMessage([]byte(encodeMessage("attach", M{"primary": primary})))
	return c, nil
}

// sendMessage queues a message for crawl and reports whether there was room.
// If crawl has stopped reading, messages are dropped rather than piling up.
func (c *crawlConn) sendMessage(data []byte) bool {
	select {
	case c.send <- data:
		return true
	default:
		return false
	}
}

func (c *crawlConn) close() {
	c.local.Close()
	os.Remove(c.localPath)
	close(c.send)
}
