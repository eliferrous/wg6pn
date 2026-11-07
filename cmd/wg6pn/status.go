package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/superfly/wg6pn/internal/flywg"
)

// status serves tunnel state as JSON on loopback.
type status struct {
	mu        sync.Mutex
	cfg       *Config
	peer      *peerState
	tunnel    *flywg.Tunnel
	lastError string
	errorAt   time.Time
}

type statusDoc struct {
	Up        bool      `json:"up"`
	Org       string    `json:"org"`
	Network   string    `json:"network,omitempty"`
	PeerName  string    `json:"peer_name,omitempty"`
	DNSName   string    `json:"dns_name,omitempty"`
	PeerIP    string    `json:"peer_ip,omitempty"`
	Gateway   string    `json:"gateway,omitempty"`
	Addrs     []string  `json:"addrs,omitempty"`
	Forwards  []Forward `json:"forwards"`
	LastError string    `json:"last_error,omitempty"`
	ErrorAt   time.Time `json:"last_error_at,omitzero"`
}

func newStatus(cfg *Config) *status { return &status{cfg: cfg} }

func (s *status) setPeer(p *peerState) {
	s.mu.Lock()
	s.peer = p
	s.mu.Unlock()
}

func (s *status) setTunnel(t *flywg.Tunnel) {
	s.mu.Lock()
	s.tunnel = t
	s.mu.Unlock()
}

func (s *status) setError(err error) {
	s.mu.Lock()
	s.tunnel = nil
	s.lastError = err.Error()
	s.errorAt = time.Now().UTC()
	s.mu.Unlock()
}

func (s *status) doc() statusDoc {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := statusDoc{
		Org:       s.cfg.Org,
		Network:   s.cfg.Network,
		Forwards:  s.cfg.Forwards,
		LastError: s.lastError,
		ErrorAt:   s.errorAt,
	}
	if d.Forwards == nil {
		d.Forwards = []Forward{}
	}
	if p := s.peer; p != nil {
		d.PeerName = p.Name
		d.DNSName = p.Name + "._peer.internal"
		d.PeerIP = p.PeerIP
		d.Gateway = p.Endpoint
	}
	if t := s.tunnel; t != nil {
		select {
		case <-t.Done():
		default:
			d.Up = true
		}
		for _, a := range t.Addrs() {
			d.Addrs = append(d.Addrs, a.String())
		}
	}
	return d
}

func (s *status) serve(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s.doc())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !s.doc().Up {
			http.Error(w, "tunnel down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
