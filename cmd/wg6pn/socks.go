package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

func serveSOCKS5(ctx context.Context, ln net.Listener, dial dialFunc) {
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			if err := handleSOCKS5(ctx, c, dial); err != nil {
				log.Printf("socks: %v", err)
			}
		}()
	}
}

const (
	socksVer        = 5
	socksCmdConnect = 1
	atypIPv4        = 1
	atypDomain      = 3
	atypIPv6        = 4

	repSuccess          = 0
	repGeneralFailure   = 1
	repCmdNotSupported  = 7
	repAtypNotSupported = 8
)

func handleSOCKS5(ctx context.Context, c net.Conn, dial dialFunc) error {
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReader(c)

	// greeting: VER NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return err
	}
	if hdr[0] != socksVer {
		return fmt.Errorf("version %d", hdr[0])
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(r, methods); err != nil {
		return err
	}
	noAuth := false
	for _, m := range methods {
		if m == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		_, _ = c.Write([]byte{socksVer, 0xff})
		return errors.New("client offered no acceptable auth method")
	}
	if _, err := c.Write([]byte{socksVer, 0}); err != nil {
		return err
	}

	// request: VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(r, req); err != nil {
		return err
	}
	if req[1] != socksCmdConnect {
		reply(c, repCmdNotSupported)
		return fmt.Errorf("command %d not supported", req[1])
	}
	var host string
	switch req[3] {
	case atypIPv4, atypIPv6:
		n := 4
		if req[3] == atypIPv6 {
			n = 16
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return err
		}
		host = net.IP(b).String()
	case atypDomain:
		l, err := r.ReadByte()
		if err != nil {
			return err
		}
		b := make([]byte, l)
		if _, err := io.ReadFull(r, b); err != nil {
			return err
		}
		host = string(b)
	default:
		reply(c, repAtypNotSupported)
		return fmt.Errorf("address type %d not supported", req[3])
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(r, pb); err != nil {
		return err
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	up, err := dial(dctx, "tcp", target)
	cancel()
	if err != nil {
		reply(c, repGeneralFailure)
		return fmt.Errorf("dial %s: %w", target, err)
	}
	defer up.Close()
	reply(c, repSuccess)
	_ = c.SetDeadline(time.Time{})

	// r may already hold bytes sent after the request
	pipe(&bufferedConn{Conn: c, r: r}, up)
	return nil
}

func reply(c net.Conn, rep byte) {
	// the bound address is meaningless through the tunnel
	_, _ = c.Write(append([]byte{socksVer, rep, 0, atypIPv6}, make([]byte, 18)...))
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return b.Conn.Close()
}
