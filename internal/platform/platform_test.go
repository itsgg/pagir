package platform

import (
	"path/filepath"
	"testing"
)

func TestTryLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.lock")
	unlock, ok, err := TryLock(p)
	if err != nil || !ok {
		t.Fatalf("first lock: ok %v err %v", ok, err)
	}
	if _, ok, err := TryLock(p); err != nil || ok {
		t.Fatalf("second lock while held: ok %v err %v", ok, err)
	}
	unlock()
	again, ok, err := TryLock(p)
	if err != nil || !ok {
		t.Fatalf("lock after release: ok %v err %v", ok, err)
	}
	again()
}
