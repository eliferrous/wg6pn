package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func fakeGraphQL(t *testing.T, addErr string) (*httptest.Server, *[]string, *[]gqlReq) {
	t.Helper()
	var auths []string // Authorization headers seen
	var reqs []gqlReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q gqlReq
		_ = json.NewDecoder(r.Body).Decode(&q)
		auths = append(auths, r.Header.Get("Authorization"))
		reqs = append(reqs, q)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q.Query, "organization(slug"):
			_, _ = w.Write([]byte(`{"data":{"organization":{"id":"org-node-1"}}}`))
		case addErr != "":
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"` + addErr + `"}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"addWireGuardPeer":{"peerip":"fdaa:0:18:a7b:1:2:3:2","endpointip":"203.0.113.7","pubkey":"gwkey"}}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &auths, &reqs
}

func TestRegisterSendsNoToken(t *testing.T) {
	srv, auths, reqs := fakeGraphQL(t, "")
	reg := newGraphQLRegistrar(srv.URL)

	p, err := reg.register(context.Background(), "my-org", "net1", "", "deku", "pubkey=")
	if err != nil {
		t.Fatal(err)
	}
	if p.PeerIP != "fdaa:0:18:a7b:1:2:3:2" || p.Endpoint != "203.0.113.7" || p.GatewayPubkey != "gwkey" {
		t.Fatalf("peer %+v", p)
	}
	for _, a := range *auths {
		if a != "" {
			t.Fatalf("sent Authorization %q; the connector supplies it", a)
		}
	}
	in := (*reqs)[1].Variables["input"].(map[string]any)
	if in["organizationId"] != "org-node-1" || in["name"] != "deku" || in["pubkey"] != "pubkey=" || in["network"] != "net1" {
		t.Fatalf("mutation input %v", in)
	}
	if _, ok := in["region"]; ok {
		t.Error("empty region should be omitted")
	}
}

func TestRegisterSurfacesGraphQLErrors(t *testing.T) {
	srv, _, _ := fakeGraphQL(t, "Name has already been taken")
	reg := newGraphQLRegistrar(srv.URL)
	_, err := reg.register(context.Background(), "o", "", "", "deku", "k")
	if err == nil || !strings.Contains(err.Error(), "already been taken") {
		t.Fatalf("got %v", err)
	}
}
