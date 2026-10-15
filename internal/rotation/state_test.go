package rotation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opencode2api/internal/config"
)

// TestFlushBatchesHotPathWrites pins the batching contract: attempt records
// only mark the state dirty, and nothing reaches disk until Flush runs.
func TestFlushBatchesHotPathWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.rotation.json")
	manager, err := New(path, config.RotationConfig{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("state file after construction: %v", err)
	}

	manager.Record(Attempt{Request: "req-1", Alias: "sota", Model: "m1", Number: 1, Outcome: "success"})
	manager.Fail(Token{Group: "sota", Model: "m1"}, 0, "probe")
	afterRecord, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(afterRecord) != string(before) {
		t.Fatalf("hot-path mutations wrote to disk before Flush:\nbefore=%s\nafter=%s", before, afterRecord)
	}

	manager.Flush()
	flushed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(flushed), "req-1") {
		t.Fatalf("Flush did not persist the batch:\n%s", flushed)
	}

	// A clean Flush must be a no-op rather than another rewrite.
	statBefore, _ := os.Stat(path)
	manager.Flush()
	statAfter, _ := os.Stat(path)
	if statBefore.ModTime() != statAfter.ModTime() {
		t.Fatal("a clean Flush rewrote the state file")
	}
	if manager.PersistenceError() != "" {
		t.Fatalf("unexpected persistence error: %s", manager.PersistenceError())
	}
}

func TestFlushRetriesAfterAFailedWrite(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "state.rotation.json")
	manager, err := New(path, config.RotationConfig{})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Make the directory read-only so the next write fails, then restore it.
	manager.Record(Attempt{Request: "req-fail", Alias: "sota", Model: "m1", Number: 1, Outcome: "success"})
	target := filepath.Join(directory, "missing")
	manager.mu.Lock()
	manager.path = filepath.Join(target, "state.json")
	manager.mu.Unlock()
	manager.Flush()
	if manager.PersistenceError() == "" {
		t.Fatal("a failed write must surface through PersistenceError")
	}
	manager.mu.Lock()
	manager.path = path
	manager.mu.Unlock()
	manager.Flush()
	if manager.PersistenceError() != "" {
		t.Fatalf("a successful retry must clear the error: %s", manager.PersistenceError())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "req-fail") {
		t.Fatalf("retry did not persist the state:\n%s", raw)
	}
}
