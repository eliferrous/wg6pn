package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func startSOCKS(t *testing.T, dial dialFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go serveSOCKS5(ctx, ln, dial)
	return ln.Addr().String()
}

func socksConnect(t *testing.T, proxy, host string, port uint16) (net.Conn, byte) {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxy, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	g := make([]byte, 2)
	if _, err := io.ReadFull(c, g); err != nil || g[1] != 0 {
		t.Fatalf("greeting reply %v %v", g, err)
	}
	req := []byte{5, 1, 0, 3, byte(len(host))}
	req = append(req, host...)
	req = binary.BigEndian.AppendUint16(req, port)
	if _, err := c.Write(req); err != nil {
		t.Fatal(err)
	}
	rep := make([]byte, 22)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatal(err)
	}
	return c, rep[1]
}

func TestSOCKSDialsHostnameThroughDialer(t *testing.T) {
	echo := echoServer(t)
	var asked string
	proxy := startSOCKS(t, func(ctx context.Context, network, addr string) (net.Conn, error) {
		asked = addr
		var d net.Dialer
		return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(echo)))
	})

	c, rep := socksConnect(t, proxy, "my-app.internal", 5432)
	defer c.Close()
	if rep != repSuccess {
		t.Fatalf("reply %d", rep)
	}
	if asked != "my-app.internal:5432" {
		t.Fatalf("dialer got %q; hostname must reach the tunnel dialer unresolved", asked)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || !bytes.Equal(buf, []byte("ping")) {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func TestSOCKSReportsDialFailure(t *testing.T) {
	proxy := startSOCKS(t, func(context.Context, string, string) (net.Conn, error) {
		return nil, io.EOF
	})
	c, rep := socksConnect(t, proxy, "nope.internal", 1)
	c.Close()
	if rep != repGeneralFailure {
		t.Fatalf("reply %d, want general failure", rep)
	}
}

func TestSOCKSRejectsAuthOnlyClients(t *testing.T) {
	proxy := startSOCKS(t, nil)
	c, _ := net.Dial("tcp", proxy)
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write([]byte{5, 1, 2}) // username/password only
	g := make([]byte, 2)
	if _, err := io.ReadFull(c, g); err != nil || g[1] != 0xff {
		t.Fatalf("want no-acceptable-methods, got %v %v", g, err)
	}
}
