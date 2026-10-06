// Package probe checks whether a BackPack tunnel actually carries traffic.
//
// The "echo" probe is end to end: the kharej server dials a forwarded port on
// the Iran server, the Iran side sends that connection back through the tunnel
// to the echo server running here, and the random token must come back. A
// reply proves the Iran server is reachable, its BackPack server is up and the
// tunnel to this kharej server is connected and passing data.
package probe

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

const magic = "BPP1"

// Result of one probe.
type Result struct {
	OK  bool
	RTT time.Duration
	Err error
}

// Prober checks one target.
type Prober interface {
	Probe(ctx context.Context) Result
}

// New returns a prober of the given type for host:port.
func New(kind, host string, port int) (Prober, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	switch kind {
	case "echo":
		return Echo{Addr: addr}, nil
	case "tcp":
		return TCP{Addr: addr}, nil
	}
	return nil, fmt.Errorf("unknown probe type %q", kind)
}

// TCP only checks that the port accepts a connection.
type TCP struct{ Addr string }

func (p TCP) Probe(ctx context.Context) Result {
	start := time.Now()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return Result{Err: err}
	}
	c.Close()
	return Result{OK: true, RTT: time.Since(start)}
}

// Echo sends a random token through the tunnel and waits for it to return.
type Echo struct{ Addr string }

func (p Echo) Probe(ctx context.Context) Result {
	start := time.Now()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", p.Addr)
	if err != nil {
		return Result{Err: err}
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return Result{Err: err}
	}
	want := magic + " " + hex.EncodeToString(buf)
	if _, err := io.WriteString(c, want+"\n"); err != nil {
		return Result{Err: fmt.Errorf("write: %w", err)}
	}
	line, err := bufio.NewReaderSize(c, 128).ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Result{Err: errors.New("connection closed without reply (tunnel not connected?)")}
		}
		return Result{Err: fmt.Errorf("read: %w", err)}
	}
	if strings.TrimSpace(line) != want {
		return Result{Err: errors.New("wrong reply (probe port is not mapped to the echo server?)")}
	}
	return Result{OK: true, RTT: time.Since(start)}
}

// ServeEcho runs the echo endpoint the Iran servers forward the probe port to.
// It answers one BPP1 line per connection and ignores everything else.
func ServeEcho(ctx context.Context, listen string) error {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			log.Printf("echo: accept: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handleEcho(c)
	}
}

func handleEcho(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReaderSize(c, 128)
	line, err := r.ReadSlice('\n')
	if err != nil || len(line) > 100 || !strings.HasPrefix(string(line), magic+" ") {
		return
	}
	c.Write(line)
}

// Member is one named tunnel check inside a Group.
type Member struct {
	Name string
	P    Prober
}

// Group checks every tunnel of one server at once.
type Group struct {
	Members []Member
	// RequireAll fails the server when any member fails; otherwise only when all do.
	RequireAll bool
}

// Run probes all members in parallel. It returns the server-level result and
// each member's own result, in order.
func (g Group) Run(ctx context.Context) (Result, []Result) {
	each := make([]Result, len(g.Members))
	done := make(chan struct{}, len(g.Members))
	for i, m := range g.Members {
		go func() {
			each[i] = m.P.Probe(ctx)
			done <- struct{}{}
		}()
	}
	for range g.Members {
		<-done
	}
	var failed []string
	var rtt time.Duration
	okCount := 0
	for i, r := range each {
		if r.OK {
			okCount++
			rtt = max(rtt, r.RTT)
			continue
		}
		msg := "failed"
		if r.Err != nil {
			msg = r.Err.Error()
		}
		if len(g.Members) == 1 {
			failed = append(failed, msg)
		} else {
			failed = append(failed, g.Members[i].Name+": "+msg)
		}
	}
	ok := okCount == len(each)
	if !g.RequireAll {
		ok = okCount > 0
	}
	res := Result{OK: ok, RTT: rtt}
	if !ok {
		res.Err = errors.New(strings.Join(failed, "; "))
	}
	return res, each
}
