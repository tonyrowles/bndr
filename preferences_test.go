package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPreferencesCompatibilityAndAtomicUpdates(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "apps.json")
	if err := os.WriteFile(file, []byte(`{"8765":{"name":"Existing Node name","hidden":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := loadPreferences(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.get(8765); got.Name != "Existing Node name" || !got.Hidden {
		t.Fatal(got)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			name := "Research"
			hidden := true
			if err := p.update(3000+i, &name, &hidden); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	reloaded, err := loadPreferences(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.snapshot()) != 21 {
		t.Fatal("lost concurrent writes")
	}
	empty := ""
	hidden := false
	if err := reloaded.update(8765, &empty, &hidden); err != nil {
		t.Fatal(err)
	}
	if reloaded.get(8765) != (settings{}) {
		t.Fatal("reset failed")
	}
	if err := os.WriteFile(file, []byte("invalid json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPreferences(dir); err == nil {
		t.Fatal("corrupt preferences must not be overwritten")
	}
}
