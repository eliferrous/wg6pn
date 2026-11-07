package flywg

// wg6pn addition: device B stands in for a Machine and dials device A, built
// as a token-mode tunnel, over real WireGuard on loopback.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func listenPort(t *testing.T, dev *device.Device) int {
	t.Helper()
	cfg, err := dev.IpcGet()
	require.NoError(t, err)
	for _, l := range strings.Split(cfg, "\n") {
		if p, ok := strings.CutPrefix(l, "listen_port="); ok {
			var n int
			fmt.Sscan(p, &n)
			return n
		}
	}
	t.Fatal("no listen_port")
	return 0
}

func handshaked(t *testing.T, dev *device.Device) bool {
	t.Helper()
	cfg, err := dev.IpcGet()
	require.NoError(t, err)
	for _, l := range strings.Split(cfg, "\n") {
		if v, ok := strings.CutPrefix(l, "last_handshake_time_sec="); ok && v != "0" {
			return true
		}
	}
	return false
}

func hexKey(t *testing.T, b64 string) string {
	t.Helper()
	var k PrivateKey
	require.NoError(t, k.UnmarshalText([]byte(b64)))
	return k.ToHex()
}

func TestInboundOverRealWireGuard(t *testing.T) {
	aPub, aPriv := testKeypair(t)
	bPub, bPriv := testKeypair(t)

	const (
		aPeer    = "fdaa:0:18:a7b:1:1111:2222:3302"
		machine  = "fdaa:0:18:a7b:9:aaaa:bbbb:2"
		listenOn = 8080
	)

	// --- device B, the "Machine": owns its address, routes A's /120 to A
	bTun, bNet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(machine)}, nil, 1420)
	require.NoError(t, err)
	bDev := device.NewDevice(bTun, conn.NewDefaultBind(), device.NewLogger(device.LogLevelError, "B "))
	defer bDev.Close()
	require.NoError(t, bDev.IpcSet("private_key="+hexKey(t, bPriv)+"\n"))
	require.NoError(t, bDev.Up())

	// --- device A's config, built the way ConnectToken builds it
	state := &WireGuardState{
		Org: "o", LocalPublic: aPub, LocalPrivate: aPriv,
	}
	prov := &TokenProvision{GatewayPubkey: bPub, PeerIP: aPeer, DNS: "fdaa:0:18::3"}
	applyProvision(state, "127.0.0.1", prov)
	cfg := tokenTunnelConfig(state, prov)

	// Like the gateway, B knows A before A speaks and learns A's endpoint by
	// roaming.
	pk := PublicKey{}
	require.NoError(t, pk.UnmarshalText([]byte(aPub)))
	require.NoError(t, bDev.IpcSet(fmt.Sprintf("public_key=%s\nallowed_ip=%s\n", pk.ToHex(), cfg.LocalNetwork)))

	aDev, _, aNet, err := startDevice(cfg, fmt.Sprintf("127.0.0.1:%d", listenPort(t, bDev)))
	require.NoError(t, err)
	defer aDev.Close()

	// A listens the way Tunnel.ListenTCP does
	ln, err := aNet.ListenTCPAddrPort(netip.AddrPortFrom(netip.Addr{}, listenOn))
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "%s via %s", line, c.LocalAddr())
			}()
		}
	}()

	// dialing before A's handshake lands makes B start a crossing handshake,
	// which takes a 5s retry to settle
	deadline := time.Now().Add(5 * time.Second)
	for !handshaked(t, bDev) {
		require.True(t, time.Now().Before(deadline), "no handshake from A")
		time.Sleep(10 * time.Millisecond)
	}

	for _, dst := range []string{aPeer, cfg.LocalNetwork.IP.String()} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := bNet.DialContext(ctx, "tcp", net.JoinHostPort(dst, fmt.Sprint(listenOn)))
		cancel()
		require.NoError(t, err, "machine -> sprite %s", dst)
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(c, "hi\n")
		got, _ := io.ReadAll(c)
		c.Close()
		require.True(t, bytes.HasPrefix(got, []byte("hi\n via ")), "dst %s got %q", dst, got)
		t.Logf("machine -> [%s]:%d ok: %q", dst, listenOn, got)
	}
}

// wg6pn addition: the device must send unprompted, or ws2wg drops inbound.
func TestTokenDeviceSpeaksFirst(t *testing.T) {
	gwPub, _ := testKeypair(t)
	pub, priv := testKeypair(t)

	// stand-in for the plugboard: a UDP socket that records the first packet
	plug, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer plug.Close()

	prov := &TokenProvision{GatewayPubkey: gwPub, PeerIP: "fdaa:0:18:a7b:1:1111:2222:3302", DNS: "fdaa:0:18::3"}
	state := &WireGuardState{Org: "o", LocalPublic: pub, LocalPrivate: priv}
	applyProvision(state, "127.0.0.1", prov)
	cfg := tokenTunnelConfig(state, prov)
	require.Positive(t, cfg.KeepAlive, "token tunnels need a persistent keepalive")

	dev, _, _, err := startDevice(cfg, plug.LocalAddr().String())
	require.NoError(t, err)
	defer dev.Close()

	_ = plug.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, _, err := plug.ReadFrom(buf)
	require.NoError(t, err, "device sent nothing after coming up; inbound would be dropped")
	// WireGuard message type 1 is a handshake initiation
	require.Equal(t, byte(1), buf[0], "first packet type (len %d)", n)
}

// wg6pn addition: API-registered peers need the same keepalive and address.
func TestLegacyDeviceSpeaksFirst(t *testing.T) {
	gwPub, _ := testKeypair(t)
	pub, priv := testKeypair(t)

	plug, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer plug.Close()

	state := &WireGuardState{Org: "o", LocalPublic: pub, LocalPrivate: priv}
	state.Peer.Peerip = "fdaa:0:18:a7b:1:1111:2222:3302"
	state.Peer.Endpointip = "127.0.0.1"
	state.Peer.Pubkey = gwPub
	cfg := legacyTunnelConfig(state)
	require.Positive(t, cfg.KeepAlive)
	require.Equal(t, "fdaa:0:18:a7b:1:1111:2222:3302", cfg.PeerAddr.String())

	dev, _, _, err := startDevice(cfg, plug.LocalAddr().String())
	require.NoError(t, err)
	defer dev.Close()

	_ = plug.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	_, _, err = plug.ReadFrom(buf)
	require.NoError(t, err, "device sent nothing after coming up")
	require.Equal(t, byte(1), buf[0])
}
