//go:build cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/types"
)

// TestListRepoPeerDetection tests DetectPeerBackend for various backend types.
func TestListRepoPeerDetection(t *testing.T) {
	t.Parallel()

	t.Run("DetectPeerBackend finds dolt", func(t *testing.T) {
		dir := t.TempDir()
		doltDir := filepath.Join(dir, ".beads", "dolt")
		if err := os.MkdirAll(doltDir, 0755); err != nil {
			t.Fatal(err)
		}

		backend, err := dolt.DetectPeerBackend(dir)
		if err != nil {
			t.Fatalf("DetectPeerBackend failed: %v", err)
		}
		if backend != dolt.PeerBackendDolt {
			t.Errorf("expected dolt backend, got %s", backend)
		}
	})

	t.Run("DetectPeerBackend rejects missing beads dir", func(t *testing.T) {
		emptyDir := t.TempDir()
		_, err := dolt.DetectPeerBackend(emptyDir)
		if err == nil {
			t.Fatal("expected error for directory without .beads")
		}
	})

	t.Run("DetectPeerBackend finds jsonl", func(t *testing.T) {
		jsonlDir := t.TempDir()
		beadsDir := filepath.Join(jsonlDir, ".beads")
		if err := os.MkdirAll(beadsDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}

		backend, err := dolt.DetectPeerBackend(jsonlDir)
		if err != nil {
			t.Fatalf("DetectPeerBackend failed: %v", err)
		}
		if backend != dolt.PeerBackendJSONL {
			t.Errorf("expected jsonl backend, got %s", backend)
		}
	})

	t.Run("DetectPeerBackend prefers dolt over jsonl", func(t *testing.T) {
		dir := t.TempDir()
		beadsDir := filepath.Join(dir, ".beads")
		doltDir := filepath.Join(beadsDir, "dolt")
		if err := os.MkdirAll(doltDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}

		backend, err := dolt.DetectPeerBackend(dir)
		if err != nil {
			t.Fatalf("DetectPeerBackend failed: %v", err)
		}
		if backend != dolt.PeerBackendDolt {
			t.Errorf("expected dolt backend (preferred), got %s", backend)
		}
	})

	t.Run("QueryPeerIssues rejects nonexistent path", func(t *testing.T) {
		_, err := dolt.QueryPeerIssues(context.Background(), "/nonexistent/path/to/repo", types.IssueFilter{})
		if err == nil {
			t.Fatal("expected error for nonexistent peer path")
		}
	})
}
