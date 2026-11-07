// wg6pn joins a Fly.io org's private network from a sprite. It registers a
// WireGuard peer through a sprites connector that holds the token, then runs
// WireGuard in userspace. It forwards inbound ports to 127.0.0.1 and serves a
// loopback SOCKS5 proxy for outbound traffic.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/eliferrous/wg6pn/internal/flywg"
)

func main() {
	check := flag.Bool("check", false, "validate configuration and exit without connecting")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *check {
		fmt.Printf("ok: org=%s network=%q peer=%s forwards=%v proxy=%s status=%s key=%s\n",
			cfg.Org, cfg.Network, peerName(os.Getenv), cfg.Forwards, cfg.ProxyAddr, cfg.StatusAddr, cfg.KeyFile)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := newStatus(cfg)
	var current atomic.Pointer[flywg.Tunnel]

	if cfg.ProxyAddr != "off" {
		ln, err := net.Listen("tcp", cfg.ProxyAddr)
		if err != nil {
			log.Fatalf("proxy listen: %v", err)
		}
		log.Printf("socks5 proxy on %s", cfg.ProxyAddr)
		go serveSOCKS5(ctx, ln, func(ctx context.Context, network, addr string) (net.Conn, error) {
			t := current.Load()
			if t == nil {
				return nil, errors.New("tunnel not up")
			}
			return t.DialContext(ctx, network, addr)
		})
	}
	if cfg.StatusAddr != "off" {
		go func() {
			if err := st.serve(ctx, cfg.StatusAddr); err != nil {
				log.Printf("status server: %v", err)
			}
		}()
	}

	peer := registerUntilDone(ctx, cfg, st)
	if peer == nil {
		return
	}

	b := backoff{min: 2 * time.Second, max: 2 * time.Minute}
	for ctx.Err() == nil {
		t, err := connectPeer(ctx, peer)
		if err != nil {
			st.setError(err)
			wait := b.next()
			log.Printf("connect: %v (retrying in %s)", err, wait)
			if !sleepCtx(ctx, wait) {
				break
			}
			continue
		}
		b.reset()
		current.Store(t)
		st.setTunnel(t)

		runCtx, cancel := context.WithCancel(ctx)
		for _, f := range cfg.Forwards {
			go serveForward(runCtx, t.ListenTCP, (&net.Dialer{}).DialContext, f)
		}

		select {
		case <-ctx.Done():
		case <-t.Done():
			log.Printf("tunnel down: %v", t.Err())
			st.setError(fmt.Errorf("tunnel down: %w", t.Err()))
		}
		cancel()
		current.Store(nil)
		_ = t.Close()
	}
	log.Printf("shutting down")
}

// registerUntilDone loads or creates this sprite's peer, retrying failures.
func registerUntilDone(ctx context.Context, cfg *Config, st *status) *registeredState {
	reg := newGraphQLRegistrar(cfg.RegisterURL)
	ident, name := spriteIdentity(os.Getenv), peerName(os.Getenv)
	b := backoff{min: 5 * time.Second, max: 10 * time.Minute}
	for ctx.Err() == nil {
		peer, priv, err := ensurePeer(ctx, cfg, ident, name, reg)
		if err == nil {
			st.setPeer(peer)
			log.Printf("peer %s at %s via gateway %s", peer.Name, peer.PeerIP, peer.Endpoint)
			return &registeredState{peerState: *peer, priv: priv}
		}
		st.setError(err)
		wait := b.next()
		log.Printf("register: %v (retrying in %s)", err, wait)
		if !sleepCtx(ctx, wait) {
			break
		}
	}
	return nil
}

type registeredState struct {
	peerState
	priv string
}

func connectPeer(ctx context.Context, p *registeredState) (*flywg.Tunnel, error) {
	state := &flywg.WireGuardState{
		Org:          p.Org,
		Name:         p.Name,
		LocalPublic:  p.PublicKey,
		LocalPrivate: p.priv,
	}
	state.Peer.Peerip = p.PeerIP
	state.Peer.Endpointip = p.Endpoint
	state.Peer.Pubkey = p.GatewayPubkey
	log.Printf("connecting to %s as %s", p.Endpoint, p.Name)

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return flywg.ConnectWS(cctx, state)
}

type backoff struct {
	min, max, cur time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur *= 2
		if b.cur > b.max {
			b.cur = b.max
		}
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }
