package probe

import (
	"context"
	"net"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

func TestEchoRoundTrip(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ServeEcho(ctx, addr)
	time.Sleep(50 * time.Millisecond)

	pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	defer pcancel()
	if r := (Echo{Addr: addr}).Probe(pctx); !r.OK {
		t.Fatalf("echo probe failed: %v", r.Err)
	}
}

func TestEchoFailsWhenNothingAnswers(t *testing.T) {
	// A listener that accepts and closes, like an Iran server whose tunnel has no client.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if r := (Echo{Addr: ln.Addr().String()}).Probe(ctx); r.OK {
		t.Fatal("echo probe must fail when the connection is dropped")
	}
	// The weaker TCP probe cannot tell the difference.
	if r := (TCP{Addr: ln.Addr().String()}).Probe(ctx); !r.OK {
		t.Fatalf("tcp probe: %v", r.Err)
	}
}

func TestEchoFailsOnSilence(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold open, never answer
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if r := (Echo{Addr: ln.Addr().String()}).Probe(ctx); r.OK {
		t.Fatal("silent peer must fail")
	}
}
