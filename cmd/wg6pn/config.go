package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Forward struct {
	PeerPort  int `json:"peer_port"`
	LocalPort int `json:"local_port"`
}

type Config struct {
	Org         string
	Network     string
	RegisterURL string
	Region      string
	KeyFile     string
	StateFile   string
	Forwards    []Forward
	ProxyAddr   string
	StatusAddr  string
}

func configFromEnv(getenv func(string) string) (*Config, error) {
	c := &Config{
		Org:         getenv("WG6PN_ORG"),
		Network:     getenv("WG6PN_NETWORK"),
		RegisterURL: getenv("WG6PN_REGISTER_URL"),
		Region:      getenv("WG6PN_REGION"),
		KeyFile:     getenv("WG6PN_KEY_FILE"),
		StateFile:   getenv("WG6PN_STATE_FILE"),
		ProxyAddr:   getenv("WG6PN_PROXY"),
		StatusAddr:  getenv("WG6PN_STATUS"),
	}
	if c.ProxyAddr == "" {
		c.ProxyAddr = "127.0.0.1:1080"
	}
	if c.StatusAddr == "" {
		c.StatusAddr = "127.0.0.1:9180"
	}
	if c.KeyFile == "" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("WG6PN_KEY_FILE unset and no config dir: %w", err)
		}
		c.KeyFile = filepath.Join(dir, "wg6pn", "key")
	}
	if c.StateFile == "" {
		c.StateFile = filepath.Join(filepath.Dir(c.KeyFile), "peer.json")
	}
	if c.Org == "" {
		return nil, errors.New("WG6PN_ORG is required (the org's real slug, not \"personal\")")
	}
	if c.RegisterURL == "" {
		return nil, errors.New("WG6PN_REGISTER_URL is required: the connector's GraphQL URL")
	}

	fwds, err := parseForwards(getenv("WG6PN_FORWARD"))
	if err != nil {
		return nil, err
	}
	c.Forwards = fwds

	for name, addr := range map[string]string{"WG6PN_PROXY": c.ProxyAddr, "WG6PN_STATUS": c.StatusAddr} {
		if addr == "off" {
			continue
		}
		if err := requireLoopback(addr); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}

	return c, nil
}

// parseForwards reads "5432,8080:3000". ';' also separates items because
// sprite-env splits --env values on commas.
func parseForwards(s string) ([]Forward, error) {
	var out []Forward
	seen := map[int]bool{}
	items := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' })
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		peer, local, found := strings.Cut(item, ":")
		if !found {
			local = peer
		}
		p, err := parsePort(peer)
		if err != nil {
			return nil, fmt.Errorf("WG6PN_FORWARD %q: %w", item, err)
		}
		l, err := parsePort(local)
		if err != nil {
			return nil, fmt.Errorf("WG6PN_FORWARD %q: %w", item, err)
		}
		if seen[p] {
			return nil, fmt.Errorf("WG6PN_FORWARD: peer port %d listed twice", p)
		}
		seen[p] = true
		out = append(out, Forward{PeerPort: p, LocalPort: l})
	}
	return out, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("bad port %q", s)
	}
	return n, nil
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q must be a loopback IP address", addr)
	}
	return nil
}
