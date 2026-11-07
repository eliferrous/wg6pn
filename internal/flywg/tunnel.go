package flywg

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/netip"
	"sync"

	"github.com/miekg/dns"
	fly "github.com/superfly/fly-go"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const DefaultTokenGateway = "gateway.machines.dev"

type Tunnel struct {
	// mu guards everything below it. Token-mode tunnels replace their
	// device, netstack and addresses when an anycast reconnect lands on a
	// different gateway; legacy tunnels never change after construction.
	mu     sync.RWMutex
	dev    *device.Device
	tun    tun.Device
	net    *netstack.Net
	dnsIP  net.IP
	State  *WireGuardState
	Config *Config

	// endpointAddr is what the WireGuard device sends to: the gateway's
	// UDP address, or the local websocket proxy's plugboard port.
	endpointAddr string
	wscancel     func()
	resolv       *net.Resolver

	// token mode
	ephemeral bool
	wswg      *WsWgProxy
	done      chan struct{}
	doneOnce  sync.Once
	err       error

	// rebuilt closes when reprovision swaps the netstack (wg6pn addition).
	rebuilt chan struct{}
}

func Connect(ctx context.Context, state *WireGuardState) (*Tunnel, error) {
	return doConnect(ctx, state, false)
}

func doConnect(ctx context.Context, state *WireGuardState, wswg bool) (*Tunnel, error) {
	cfg := legacyTunnelConfig(state)

	endpointHost, endpointPort, err := net.SplitHostPort(cfg.Endpoint)
	if err != nil {
		return nil, err
	}

	endpointIPs, err := net.LookupIP(endpointHost)
	if err != nil {
		return nil, err
	}

	endpointIP := endpointIPs[rand.Intn(len(endpointIPs))]
	endpointAddr := net.JoinHostPort(endpointIP.String(), endpointPort)

	var wscancel context.CancelFunc
	if wswg {
		lifetimeCtx, cancel := context.WithCancelCause(context.Background())
		wscancel = func() { cancel(fmt.Errorf("WireGuard websocket tunnel closed: %w", context.Canceled)) }
		port, err := websocketConnect(ctx, lifetimeCtx, endpointHost)
		if err != nil {
			wscancel()

			return nil, err
		}

		endpointAddr = fmt.Sprintf("127.0.0.1:%d", port)
	}

	tunnel, err := newTunnel(state, cfg, endpointAddr, wscancel)
	if err != nil {
		if wscancel != nil {
			wscancel()
		}

		return nil, err
	}

	return tunnel, nil
}

// ConnectToken establishes a WireGuard tunnel to a token-provisioning
// gateway. There's no pre-existing peer: the state's keys plus the auth
// packet are presented over the websocket and the gateway allocates our
// address inline. The state's Peer and DNS are filled in from the gateway's
// answer (and updated again if an anycast reconnect re-provisions us).
func ConnectToken(ctx context.Context, state *WireGuardState, endpoint string, auth *TokenAuth) (*Tunnel, error) {
	auth.Pubkey = state.LocalPublic
	if state.Name == "" {
		state.Name = TokenPeerName(state.LocalPublic)
	}

	lifetimeCtx, wscancel := context.WithCancel(context.Background())

	wswg, err := websocketConnectAuth(ctx, lifetimeCtx, endpoint, auth)
	if err != nil {
		wscancel()

		return nil, err
	}

	// the proxy isn't running yet, so its state is ours to read and the
	// callbacks are in place before any reconnect can happen
	port, err := wswg.Port()
	if err != nil {
		wscancel()
		wswg.close()

		return nil, err
	}

	prov := wswg.prov
	applyProvision(state, endpoint, prov)

	tunnel, err := newTunnel(state, tokenTunnelConfig(state, prov), fmt.Sprintf("127.0.0.1:%d", port), wscancel)
	if err != nil {
		wscancel()
		wswg.close()

		return nil, err
	}

	tunnel.ephemeral = true
	tunnel.wswg = wswg

	wswg.setCallbacks(tunnel.reprovision, tunnel.fail)
	wswg.start(lifetimeCtx, endpoint)

	return tunnel, nil
}

// PersistentKeepAlive is in seconds (wg6pn addition).
var PersistentKeepAlive = 25

// setPeerAddr adds the provisioned peer IP to the netstack (wg6pn addition).
func setPeerAddr(cfg *Config, peerIP string) {
	if ip := net.ParseIP(peerIP); ip != nil && (*net.IPNet)(cfg.LocalNetwork).Contains(ip) {
		cfg.PeerAddr = ip
	}
}

// legacyTunnelConfig is the config for an API-registered peer, with the
// keepalive and peer address token tunnels get (wg6pn addition).
func legacyTunnelConfig(state *WireGuardState) *Config {
	cfg := state.TunnelConfig()
	cfg.KeepAlive = PersistentKeepAlive
	setPeerAddr(cfg, state.Peer.Peerip)

	return cfg
}

func applyProvision(state *WireGuardState, endpoint string, prov *TokenProvision) {
	state.Peer = fly.CreatedWireGuardPeer{
		Peerip:     prov.PeerIP,
		Endpointip: endpoint,
		Pubkey:     prov.GatewayPubkey,
	}
	state.DNS = prov.DNS
}

func tokenTunnelConfig(state *WireGuardState, prov *TokenProvision) *Config {
	cfg := state.TunnelConfig()

	// the gateway tells us where the network's resolver is; the derived
	// fdaa:net::3 default is the same thing, but trust the gateway
	if ip := net.ParseIP(prov.DNS); ip != nil {
		cfg.DNS = ip
	}

	setPeerAddr(cfg, prov.PeerIP)

	// wg6pn addition: ws2wg drops gateway frames until the device has sent
	// once, so a listen-only peer would never be reachable. The keepalive
	// makes the device send at startup.
	cfg.KeepAlive = PersistentKeepAlive

	return cfg
}

func newTunnel(state *WireGuardState, cfg *Config, endpointAddr string, wscancel context.CancelFunc) (*Tunnel, error) {
	dev, tunDev, gNet, err := startDevice(cfg, endpointAddr)
	if err != nil {
		return nil, err
	}

	t := &Tunnel{
		dev:          dev,
		tun:          tunDev,
		net:          gNet,
		dnsIP:        cfg.DNS,
		Config:       cfg,
		State:        state,
		endpointAddr: endpointAddr,
		wscancel:     wscancel,
		done:         make(chan struct{}),
		rebuilt:      make(chan struct{}),
	}

	t.resolv = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			gNet, dnsIP, err := t.netstack()
			if err != nil {
				return nil, err
			}

			return gNet.DialContext(ctx, "tcp", net.JoinHostPort(dnsIP.String(), "53"))
		},
	}

	return t, nil
}

// startDevice builds a userspace WireGuard device (and the netstack behind
// it) for cfg, pointed at endpointAddr, and brings it up.
func startDevice(cfg *Config, endpointAddr string) (*device.Device, tun.Device, *netstack.Net, error) {
	addr, ok := netip.AddrFromSlice(cfg.LocalNetwork.IP)
	if !ok {
		return nil, nil, nil, fmt.Errorf("could not generate local network addr from IP %s: ", cfg.LocalNetwork.IP)
	}

	dnsIP, ok := netip.AddrFromSlice(cfg.DNS)
	if !ok {
		return nil, nil, nil, fmt.Errorf("could not generate DNS addr from IP %s: ", cfg.DNS)
	}

	mtu := cfg.MTU
	if mtu == 0 {
		mtu = device.DefaultMTU
	}

	// the network address stays first, so outbound traffic keeps the
	// source address flyctl has always used
	addrs := []netip.Addr{addr}
	if peer, ok := netip.AddrFromSlice(cfg.PeerAddr); ok && peer != addr {
		addrs = append(addrs, peer)
	}

	tunDev, gNet, err := netstack.CreateNetTUN(addrs, []netip.Addr{dnsIP}, mtu)
	if err != nil {
		return nil, nil, nil, err
	}

	wgDev := device.NewDevice(tunDev, conn.NewDefaultBind(), device.NewLogger(cfg.LogLevel, "(fly-ssh) "))

	wgConf := bytes.NewBuffer(nil)
	fmt.Fprintf(wgConf, "private_key=%s\n", cfg.LocalPrivateKey.ToHex())
	fmt.Fprintf(wgConf, "public_key=%s\n", cfg.RemotePublicKey.ToHex())
	fmt.Fprintf(wgConf, "endpoint=%s\n", endpointAddr)
	fmt.Fprintf(wgConf, "allowed_ip=%s\n", cfg.RemoteNetwork)
	fmt.Fprintf(wgConf, "persistent_keepalive_interval=%d\n", cfg.KeepAlive)

	if err := wgDev.IpcSetOperation(bufio.NewReader(wgConf)); err != nil {
		wgDev.Close()

		return nil, nil, nil, err
	}
	wgDev.Up()

	return wgDev, tunDev, gNet, nil
}

var errTunnelClosed = errors.New("tunnel is closed")

// RebuildError is why a token-mode tunnel died when the WireGuard device
// couldn't be rebuilt around a re-provisioned peer. It's a client-side
// failure, unlike the gateway rejections that otherwise kill tunnels.
type RebuildError struct {
	PeerIP string
	Err    error
}

func (e *RebuildError) Error() string {
	return fmt.Sprintf("rebuilding tunnel around re-provisioned peer %s: %s", e.PeerIP, e.Err)
}

func (e *RebuildError) Unwrap() error { return e.Err }

// netstack returns the current network stack and resolver address.
func (t *Tunnel) netstack() (*netstack.Net, net.IP, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if t.net == nil {
		return nil, nil, errTunnelClosed
	}

	return t.net, t.dnsIP, nil
}

// reprovision rebuilds the WireGuard device around the address and gateway
// key a reconnect got us, keeping the websocket proxy (and its plugboard
// port) in place. Connections through the old device are lost: their
// source address no longer exists.
func (t *Tunnel) reprovision(prov *TokenProvision) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.dev == nil {
		return
	}

	state := *t.State
	applyProvision(&state, state.Peer.Endpointip, prov)
	cfg := tokenTunnelConfig(&state, prov)

	dev, tunDev, gNet, err := startDevice(cfg, t.endpointAddr)
	if err != nil {
		t.failLocked(&RebuildError{PeerIP: prov.PeerIP, Err: err})

		return
	}

	old := t.dev
	t.dev, t.tun, t.net, t.dnsIP = dev, tunDev, gNet, cfg.DNS
	t.State, t.Config = &state, cfg

	old.Close()

	close(t.rebuilt)
	t.rebuilt = make(chan struct{})

	log.Printf("rebuilt tunnel for %s as %s (gateway key %s)", state.Org, prov.PeerIP, prov.GatewayPubkey)
}

// fail marks a token-mode tunnel as permanently dead and closes it.
func (t *Tunnel) fail(err error) {
	t.mu.Lock()
	t.failLocked(err)
	t.mu.Unlock()

	_ = t.Close()
}

func (t *Tunnel) failLocked(err error) {
	t.doneOnce.Do(func() {
		t.err = err
		close(t.done)
	})
}

// Done is closed once the tunnel is closed, or, for token-mode tunnels,
// once the gateway has rejected it for good; Err says which.
func (t *Tunnel) Done() <-chan struct{} {
	return t.done
}

// Err returns why the tunnel died, or nil if it was closed by its owner.
func (t *Tunnel) Err() error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.err
}

// Ephemeral reports whether the peer was provisioned inline by a token-mode
// gateway (and so isn't registered with the API or in the config file).
func (t *Tunnel) Ephemeral() bool {
	return t.ephemeral
}

// Provision returns what the token gateway last allocated to us (the
// expiry moves every time the token is re-presented), or nil for legacy
// tunnels.
func (t *Tunnel) Provision() *TokenProvision {
	if t.wswg == nil {
		return nil
	}

	return t.wswg.provision()
}

// StateAndConfig returns the tunnel's current WireGuard state and config.
// Token-mode tunnels may change these on reconnect.
func (t *Tunnel) StateAndConfig() (*WireGuardState, *Config) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return t.State, t.Config
}

func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.wscancel != nil {
		t.wscancel()
		t.wscancel = nil
	}

	if t.dev != nil {
		t.dev.Close()
	}

	t.dev, t.net, t.tun = nil, nil, nil

	t.doneOnce.Do(func() { close(t.done) })

	return nil
}

func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	gNet, _, err := t.netstack()
	if err != nil {
		return nil, err
	}

	return gNet.DialContext(ctx, network, addr)
}

// ListenTCP listens on every tunnel address (wg6pn addition). The channel
// closes when a reprovision kills the listener.
func (t *Tunnel) ListenTCP(port int) (net.Listener, <-chan struct{}, error) {
	t.mu.RLock()
	gNet, rebuilt := t.net, t.rebuilt
	t.mu.RUnlock()

	if gNet == nil {
		return nil, nil, errTunnelClosed
	}

	// a zero Addr becomes gvisor's empty address: every address on the NIC
	ln, err := gNet.ListenTCPAddrPort(netip.AddrPortFrom(netip.Addr{}, uint16(port)))
	if err != nil {
		return nil, nil, fmt.Errorf("listen tunnel :%d: %w", port, err)
	}

	return ln, rebuilt, nil
}

// Addrs returns the /120 network address and the provisioned peer IP (wg6pn
// addition).
func (t *Tunnel) Addrs() []net.IP {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := []net.IP{t.Config.LocalNetwork.IP}
	if p := t.Config.PeerAddr; p != nil && !p.Equal(t.Config.LocalNetwork.IP) {
		out = append(out, p)
	}

	return out
}

func (t *Tunnel) Resolver() *net.Resolver {
	return t.resolv
}

func (t *Tunnel) LookupTXT(ctx context.Context, name string) ([]string, error) {
	var m dns.Msg
	_ = m.SetQuestion(dns.Fqdn(name), dns.TypeTXT)

	r, err := t.queryDNS(ctx, &m)
	if err != nil {
		return nil, err
	}

	results := make([]string, 0, len(r.Answer))

	for _, a := range r.Answer {
		txt := a.(*dns.TXT)

		results = append(results, txt.Txt...)
	}

	return results, nil
}

func (t *Tunnel) ListenPing() (*netstack.PingConn, error) {
	t.mu.RLock()
	gNet, cfg := t.net, t.Config
	t.mu.RUnlock()

	if gNet == nil {
		return nil, errTunnelClosed
	}

	laddr, ok := netip.AddrFromSlice(cfg.LocalNetwork.IP)

	if !ok {
		return nil, fmt.Errorf("could not generate local network addr from IP %s: ", cfg.LocalNetwork.IP)
	}

	raddr := netip.IPv6Unspecified()

	conn, err := gNet.DialPingAddr(laddr, raddr)
	if err != nil {
		return nil, fmt.Errorf("ping listener: %w", err)
	}

	return conn, nil
}

func (t *Tunnel) LookupAAAA(ctx context.Context, name string) ([]net.IP, error) {
	var m dns.Msg
	_ = m.SetQuestion(dns.Fqdn(name), dns.TypeAAAA)

	r, err := t.queryDNS(ctx, &m)
	if err != nil {
		return nil, err
	}

	results := make([]net.IP, 0, len(r.Answer))

	for _, a := range r.Answer {
		ip := a.(*dns.AAAA).AAAA
		results = append(results, ip)
	}

	return results, nil
}

func (t *Tunnel) queryDNS(ctx context.Context, msg *dns.Msg) (*dns.Msg, error) {
	client := dns.Client{
		Net: "tcp",
		Dialer: &net.Dialer{
			Resolver: t.resolv,
		},
	}

	gNet, dnsIP, err := t.netstack()
	if err != nil {
		return nil, err
	}

	c, err := gNet.DialContext(ctx, "tcp", net.JoinHostPort(dnsIP.String(), "53"))
	if err != nil {
		return nil, err
	}
	defer c.Close()

	conn := &dns.Conn{Conn: c}
	defer conn.Close()

	r, _, err := client.ExchangeWithConn(msg, conn)

	return r, err
}
