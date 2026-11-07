package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type countingRegistrar struct {
	calls int
	names []string
	err   error
}

func (c *countingRegistrar) register(_ context.Context, _, _, _, name, _ string) (*registeredPeer, error) {
	c.calls++
	c.names = append(c.names, name)
	if c.err != nil {
		return nil, c.err
	}
	return &registeredPeer{PeerIP: "fdaa::2", Endpoint: "203.0.113.7", GatewayPubkey: "gw"}, nil
}

func testCfg(t *testing.T) *Config {
	dir := t.TempDir()
	return &Config{Org: "o", KeyFile: filepath.Join(dir, "key"), StateFile: filepath.Join(dir, "peer.json")}
}

func TestEnsurePeerRegistersOnceThenReuses(t *testing.T) {
	cfg, reg := testCfg(t), &countingRegistrar{}

	st1, priv1, err := ensurePeer(context.Background(), cfg, "FLY_COMPUTE_ID=a", "deku", reg)
	if err != nil {
		t.Fatal(err)
	}
	st2, priv2, err := ensurePeer(context.Background(), cfg, "FLY_COMPUTE_ID=a", "deku", reg)
	if err != nil {
		t.Fatal(err)
	}
	if reg.calls != 1 {
		t.Fatalf("registered %d times, want 1", reg.calls)
	}
	if *st1 != *st2 || priv1 != priv2 {
		t.Fatal("second boot returned different state or key")
	}
	if fi, _ := os.Stat(cfg.StateFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("state mode %v", fi.Mode().Perm())
	}
}

func TestEnsurePeerChangeRotatesKeyAndRegisters(t *testing.T) {
	cfg, reg := testCfg(t), &countingRegistrar{}
	before, _, err := ensurePeer(context.Background(), cfg, "FLY_COMPUTE_ID=before", "before", reg)
	if err != nil {
		t.Fatal(err)
	}

	// a sprite with a different identity boots with these files
	after, _, err := ensurePeer(context.Background(), cfg, "FLY_COMPUTE_ID=after", "after", reg)
	if err != nil {
		t.Fatal(err)
	}
	if reg.calls != 2 || reg.names[1] != "after" {
		t.Fatalf("calls=%d names=%v", reg.calls, reg.names)
	}
	if after.PublicKey == before.PublicKey {
		t.Fatal("new identity kept the old key")
	}
	if after.Identity != "FLY_COMPUTE_ID=after" {
		t.Fatalf("identity %q", after.Identity)
	}
}

func TestEnsurePeerRejectsMismatchedKey(t *testing.T) {
	cfg, reg := testCfg(t), &countingRegistrar{}
	if _, _, err := ensurePeer(context.Background(), cfg, "id", "n", reg); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(cfg.KeyFile) // key lost, state kept
	if _, _, err := ensurePeer(context.Background(), cfg, "id", "n", reg); err == nil {
		t.Fatal("want error when the key no longer matches the registered peer")
	}
}

func TestEnsurePeerFailedRegistrationSavesNothing(t *testing.T) {
	cfg, reg := testCfg(t), &countingRegistrar{err: errors.New("nope")}
	if _, _, err := ensurePeer(context.Background(), cfg, "id", "n", reg); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(cfg.StateFile); !os.IsNotExist(err) {
		t.Fatal("state saved for a failed registration")
	}
}

func TestPeerName(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"WG6PN_PEER_NAME": "Explicit_Name"}, "explicit-name"},
		{map[string]string{"SPRITE_NAME": "deku"}, "deku"},
		{map[string]string{"SPRITE_NAME": "9lives"}, "sprite-9lives"},
	}
	for _, c := range cases {
		if got := peerName(env(c.env)); got != c.want {
			t.Errorf("%v: got %q want %q", c.env, got, c.want)
		}
	}
}

func TestSpriteIdentityPrefersComputeID(t *testing.T) {
	got := spriteIdentity(env(map[string]string{"FLY_COMPUTE_ID": "c1", "SPRITE_NAME": "n"}))
	if got != "FLY_COMPUTE_ID=c1" {
		t.Fatalf("got %q", got)
	}
}
