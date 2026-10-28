package handlers

import (
	"os"
	"testing"
)

func TestInitEphemeralDBUsesDisposableBackend(t *testing.T) {
	if DB != nil {
		_ = CloseDB()
	}
	if err := InitEphemeralDB(); err != nil {
		t.Fatalf("InitEphemeralDB() error = %v", err)
	}
	backend := EphemeralCacheBackend()
	if backend != "tmpfs:/dev/shm" && backend != "tmp:"+os.TempDir() {
		_ = CloseDB()
		t.Fatalf("unexpected ephemeral cache backend %q", backend)
	}
	if DB == nil {
		_ = CloseDB()
		t.Fatal("InitEphemeralDB() left DB nil")
	}
	if err := CloseDB(); err != nil {
		t.Fatalf("CloseDB() error = %v", err)
	}
	if DB != nil {
		t.Fatal("CloseDB() did not clear DB")
	}
	if got := EphemeralCacheBackend(); got != "" {
		t.Fatalf("cache backend after CloseDB() = %q, want empty", got)
	}
}
