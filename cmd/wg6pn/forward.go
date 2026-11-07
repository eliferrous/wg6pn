package main

import (
	"context"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// listenFunc's channel closes when the netstack is rebuilt.
type listenFunc func(port int) (net.Listener, <-chan struct{}, error)

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// serveForward pipes tunnel connections on f.PeerPort to 127.0.0.1:f.LocalPort,
// re-listening after each rebuild.
func serveForward(ctx context.Context, listen listenFunc, dialLocal dialFunc, f Forward) {
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.LocalPort))
	for ctx.Err() == nil {
		ln, rebuilt, err := listen(f.PeerPort)
		if err != nil {
			log.Printf("forward %d: %v", f.PeerPort, err)
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		log.Printf("forward: peer :%d -> %s", f.PeerPort, target)

		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
			case <-rebuilt:
			case <-stop:
			}
			ln.Close()
		}()

		acceptLoop(ctx, ln, dialLocal, target)
		close(stop)
	}
}

func acceptLoop(ctx context.Context, ln net.Listener, dialLocal dialFunc, target string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			up, err := dialLocal(dctx, "tcp", target)
			cancel()
			if err != nil {
				log.Printf("forward %s from %s: %v", target, c.RemoteAddr(), err)
				return
			}
			defer up.Close()
			pipe(c, up)
		}()
	}
}

// pipe copies both ways, half-closing where supported.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
