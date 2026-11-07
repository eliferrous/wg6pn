package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// registeredPeer is what addWireGuardPeer returns.
type registeredPeer struct {
	PeerIP        string `json:"peerip"`
	Endpoint      string `json:"endpointip"`
	GatewayPubkey string `json:"pubkey"`
}

type registrar interface {
	register(ctx context.Context, org, network, region, name, pubkey string) (*registeredPeer, error)
}

// graphQLRegistrar calls Fly's GraphQL API through a sprites connector,
// which authenticates the sprite and adds the token.
type graphQLRegistrar struct {
	url    string
	client *http.Client
}

func newGraphQLRegistrar(url string) *graphQLRegistrar {
	return &graphQLRegistrar{url: url, client: &http.Client{Timeout: 30 * time.Second}}
}

func (g *graphQLRegistrar) register(ctx context.Context, org, network, region, name, pubkey string) (*registeredPeer, error) {
	var o struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
	}
	if err := g.do(ctx, `query($slug: String!) { organization(slug: $slug) { id } }`,
		map[string]any{"slug": org}, &o); err != nil {
		return nil, fmt.Errorf("look up org %s: %w", org, err)
	}
	if o.Organization.ID == "" {
		return nil, fmt.Errorf("org %s not found", org)
	}

	input := map[string]any{"organizationId": o.Organization.ID, "name": name, "pubkey": pubkey}
	if network != "" {
		input["network"] = network
	}
	if region != "" {
		input["region"] = region
	}
	var m struct {
		AddWireGuardPeer registeredPeer `json:"addWireGuardPeer"`
	}
	if err := g.do(ctx, `mutation($input: AddWireGuardPeerInput!) {
  addWireGuardPeer(input: $input) { peerip endpointip pubkey }
}`, map[string]any{"input": input}, &m); err != nil {
		return nil, fmt.Errorf("add peer %s: %w", name, err)
	}
	p := m.AddWireGuardPeer
	if p.PeerIP == "" || p.Endpoint == "" || p.GatewayPubkey == "" {
		return nil, errors.New("add peer: incomplete response")
	}
	return &p, nil
}

func (g *graphQLRegistrar) do(ctx context.Context, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, len(env.Errors))
		for i, e := range env.Errors {
			msgs[i] = e.Message
		}
		return errors.New(strings.Join(msgs, "; "))
	}
	return json.Unmarshal(env.Data, out)
}
