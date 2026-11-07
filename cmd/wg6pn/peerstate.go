package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// peerState is a registered peer, bound to the sprite identity that made it.
type peerState struct {
	Identity      string `json:"identity"`
	Name          string `json:"name"`
	Org           string `json:"org"`
	Network       string `json:"network,omitempty"`
	PublicKey     string `json:"public_key"`
	PeerIP        string `json:"peer_ip"`
	Endpoint      string `json:"endpoint"`
	GatewayPubkey string `json:"gateway_pubkey"`
}

func spriteIdentity(getenv func(string) string) string {
	for _, k := range []string{"FLY_COMPUTE_ID", "SPRITE_NAME"} {
		if v := getenv(k); v != "" {
			return k + "=" + v
		}
	}
	h, _ := os.Hostname()
	return "hostname=" + h
}

var nameJunk = regexp.MustCompile(`[^a-z0-9-]+`)

func peerName(getenv func(string) string) string {
	name := getenv("WG6PN_PEER_NAME")
	if name == "" {
		name = getenv("SPRITE_NAME")
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	name = strings.Trim(nameJunk.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "sprite-" + name
	}
	return name
}

func loadPeerState(path string) (*peerState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s peerState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

func savePeerState(path string, s *peerState) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ensurePeer returns this sprite's peer and key, registering on first boot.
// State left by another identity gets a new key and a new peer.
func ensurePeer(ctx context.Context, cfg *Config, ident, name string, reg registrar) (*peerState, string, error) {
	st, err := loadPeerState(cfg.StateFile)
	if err != nil {
		return nil, "", err
	}

	if st != nil && st.Identity != ident {
		log.Printf("peer state belongs to %s, not %s; rotating key and registering anew", st.Identity, ident)
		if err := os.Remove(cfg.KeyFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}
		st = nil
	}

	pub, priv, _, err := loadOrCreateKey(cfg.KeyFile)
	if err != nil {
		return nil, "", err
	}

	if st != nil {
		if st.PublicKey != pub {
			return nil, "", fmt.Errorf("%s was registered with a different key than %s", cfg.StateFile, cfg.KeyFile)
		}
		if st.Org != cfg.Org || st.Network != cfg.Network {
			return nil, "", fmt.Errorf("%s is registered in %s/%q, config says %s/%q", cfg.StateFile, st.Org, st.Network, cfg.Org, cfg.Network)
		}
		return st, priv, nil
	}

	log.Printf("registering peer %s in %s", name, cfg.Org)
	p, err := reg.register(ctx, cfg.Org, cfg.Network, cfg.Region, name, pub)
	if err != nil {
		return nil, "", err
	}
	st = &peerState{
		Identity: ident, Name: name, Org: cfg.Org, Network: cfg.Network, PublicKey: pub,
		PeerIP: p.PeerIP, Endpoint: p.Endpoint, GatewayPubkey: p.GatewayPubkey,
	}
	if err := savePeerState(cfg.StateFile, st); err != nil {
		return nil, "", fmt.Errorf("registered %s but could not save state: %w", name, err)
	}
	return st, priv, nil
}
