package main

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeTunnel hands out loopback listeners; rebuild simulates a netstack swap.
type fakeTunnel struct {
	mu      sync.Mutex
	rebuilt chan struct{}
	addrs   chan string
}

func newFakeTunnel() *fakeTunnel {
	return &fakeTunnel{rebuilt: make(chan struct{}), addrs: make(chan string, 4)}
}

func (f *fakeTunnel) listen(int) (net.Listener, <-chan struct{}, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	ch := f.rebuilt
	f.mu.Unlock()
	f.addrs <- ln.Addr().String()
	return ln, ch, nil
}

func (f *fakeTunnel) rebuild() {
	f.mu.Lock()
	close(f.rebuilt)
	f.rebuilt = make(chan struct{})
	f.mu.Unlock()
}

func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func roundTrip(t *testing.T, addr, msg string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("got %q want %q", buf, msg)
	}
}

func TestForwardRelistensAfterRebuild(t *testing.T) {
	port := echoServer(t)
	ft := newFakeTunnel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serveForward(ctx, ft.listen, (&net.Dialer{}).DialContext, Forward{PeerPort: 5432, LocalPort: port})

	first := <-ft.addrs
	roundTrip(t, first, "hello")

	ft.rebuild()
	var second string
	select {
	case second = <-ft.addrs:
	case <-time.After(2 * time.Second):
		t.Fatal("forward did not re-listen after rebuild")
	}
	roundTrip(t, second, "again")

	// the old listener must be gone
	if c, err := net.DialTimeout("tcp", first, 200*time.Millisecond); err == nil {
		c.Close()
		t.Error("old listener still accepting after rebuild")
	}
}

func TestForwardStopsOnCancel(t *testing.T) {
	ft := newFakeTunnel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		serveForward(ctx, ft.listen, (&net.Dialer{}).DialContext, Forward{PeerPort: 1, LocalPort: 1})
		close(done)
	}()
	addr := <-ft.addrs
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveForward did not return after cancel")
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		c.Close()
		t.Error("listener still open after cancel")
	}
}
