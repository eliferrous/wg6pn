package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestKeyCreatedOnceThenReused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "key")

	pub1, priv1, created, err := loadOrCreateKey(path)
	if err != nil || !created {
		t.Fatalf("first load: created=%v err=%v", created, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v, want 0600", fi.Mode().Perm())
	}

	pub2, priv2, created, err := loadOrCreateKey(path)
	if err != nil || created {
		t.Fatalf("second load: created=%v err=%v", created, err)
	}
	if pub1 != pub2 || priv1 != priv2 {
		t.Fatal("key changed between loads")
	}
}

func TestKeyRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(path, []byte("not a key"), 0o600)
	if _, _, _, err := loadOrCreateKey(path); err == nil {
		t.Fatal("want error for a corrupt key file, not a silent new identity")
	}
}

func TestKeyConcurrentFirstRunAgrees(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	var wg sync.WaitGroup
	pubs := make([]string, 8)
	for i := range pubs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, _, _, err := loadOrCreateKey(path)
			if err != nil {
				t.Error(err)
			}
			pubs[i] = p
		}()
	}
	wg.Wait()
	for _, p := range pubs[1:] {
		if p != pubs[0] {
			t.Fatal("concurrent first runs produced different keys")
		}
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".key-*"))
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}
