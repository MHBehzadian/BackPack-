package probe

import (
	"context"
	"errors"
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

type fixed Result

func (f fixed) Probe(context.Context) Result { return Result(f) }

func TestGroup(t *testing.T) {
	ok := fixed{OK: true, RTT: 20 * time.Millisecond}
	bad := fixed{Err: errors.New("timeout")}
	g := Group{Members: []Member{{"a", ok}, {"b", bad}, {"c", fixed{OK: true, RTT: 40 * time.Millisecond}}}, RequireAll: true}
	r, each := g.Run(context.Background())
	if r.OK || r.Err == nil || r.Err.Error() != "b: timeout" || len(each) != 3 || !each[0].OK || each[1].OK {
		t.Fatalf("require all: %+v %v", r, each)
	}
	g.RequireAll = false
	r, _ = g.Run(context.Background())
	if !r.OK || r.RTT != 40*time.Millisecond {
		t.Fatalf("any: %+v", r)
	}
	g.Members = []Member{{"a", bad}, {"b", bad}}
	if r, _ = g.Run(context.Background()); r.OK {
		t.Fatal("all failed must fail")
	}
	single := Group{Members: []Member{{"only", bad}}, RequireAll: true}
	if r, _ = single.Run(context.Background()); r.Err.Error() != "timeout" {
		t.Fatalf("single member error should not be prefixed: %v", r.Err)
	}
}
