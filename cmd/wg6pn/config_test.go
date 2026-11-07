package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseForwards(t *testing.T) {
	got, err := parseForwards(" 5432, 8080:3000 ,")
	if err != nil {
		t.Fatal(err)
	}
	want := []Forward{{5432, 5432}, {8080, 3000}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	semi, err := parseForwards("5432;8080:3000")
	if err != nil || !reflect.DeepEqual(semi, want) {
		t.Fatalf("semicolons: got %v %v", semi, err)
	}
	for _, bad := range []string{"0", "70000", "abc", "80:x", "80,80"} {
		if _, err := parseForwards(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

const reg = "https://api.sprites.dev/v1/gateway/custom_api/c1/graphql"

func TestConfigRequiresOrgAndRegisterURL(t *testing.T) {
	if _, err := configFromEnv(env(map[string]string{"WG6PN_REGISTER_URL": reg})); err == nil {
		t.Error("missing org: want error")
	}
	if _, err := configFromEnv(env(map[string]string{"WG6PN_ORG": "o"})); err == nil {
		t.Error("missing register URL: want error")
	}
	c, err := configFromEnv(env(map[string]string{"WG6PN_ORG": "o", "WG6PN_REGISTER_URL": reg}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ProxyAddr != "127.0.0.1:1080" || c.StatusAddr != "127.0.0.1:9180" || filepath.Base(c.StateFile) != "peer.json" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestConfigRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:1080", "[::]:1080", "example.com:1080"} {
		_, err := configFromEnv(env(map[string]string{"WG6PN_ORG": "o", "WG6PN_REGISTER_URL": reg, "WG6PN_PROXY": addr}))
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%s: got %v, want loopback error", addr, err)
		}
	}
	if _, err := configFromEnv(env(map[string]string{"WG6PN_ORG": "o", "WG6PN_REGISTER_URL": reg, "WG6PN_PROXY": "off", "WG6PN_STATUS": "[::1]:9"})); err != nil {
		t.Errorf("off and ::1 should be allowed: %v", err)
	}
}
