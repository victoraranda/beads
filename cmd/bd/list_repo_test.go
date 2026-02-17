//go:build cgo

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/types"
)

// TestListRepoPeerQuery tests the --repo flag for querying peer Dolt databases.
func TestListRepoPeerQuery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Create a peer store with test issues
	peerDir := t.TempDir()
	peerDBPath := filepath.Join(peerDir, ".beads", "dolt")
	peerStore := newTestStoreWithPrefix(t, peerDBPath, "peer")

	peerIssue := &types.Issue{
		Title:     "Peer visible issue",
		IssueType: types.TypeTask,
		Status:    types.StatusOpen,
		Priority:  2,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := peerStore.CreateIssue(ctx, peerIssue, "test"); err != nil {
		t.Fatalf("failed to create peer issue: %v", err)
	}

	t.Run("DetectPeerBackend finds dolt", func(t *testing.T) {
		backend, err := dolt.DetectPeerBackend(peerDir)
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

	// Close peer store before opening as peer (embedded mode is single-process)
	peerStore.Close()

	t.Run("OpenPeerStore reads issues", func(t *testing.T) {
		opened, err := dolt.OpenPeerStore(ctx, peerDir)
		if err != nil {
			t.Fatalf("OpenPeerStore failed: %v", err)
		}
		defer opened.Close()

		issues, err := opened.SearchIssues(ctx, "", types.IssueFilter{})
		if err != nil {
			t.Fatalf("SearchIssues on peer failed: %v", err)
		}
		if len(issues) != 1 {
			t.Errorf("expected 1 issue from peer, got %d", len(issues))
		}
		if len(issues) > 0 && issues[0].Title != "Peer visible issue" {
			t.Errorf("expected title 'Peer visible issue', got %q", issues[0].Title)
		}
	})

	t.Run("QueryPeerIssues sets source_repo", func(t *testing.T) {
		issues, err := dolt.QueryPeerIssues(ctx, peerDir, types.IssueFilter{})
		if err != nil {
			t.Fatalf("QueryPeerIssues failed: %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("expected 1 issue, got %d", len(issues))
		}
		if issues[0].SourceRepo != peerDir {
			t.Errorf("expected SourceRepo=%s, got %s", peerDir, issues[0].SourceRepo)
		}
	})

	t.Run("QueryPeerIssues with type filter", func(t *testing.T) {
		bugType := types.TypeBug
		issues, err := dolt.QueryPeerIssues(ctx, peerDir, types.IssueFilter{
			IssueType: &bugType,
		})
		if err != nil {
			t.Fatalf("QueryPeerIssues with filter failed: %v", err)
		}
		if len(issues) != 0 {
			t.Errorf("expected 0 bug issues, got %d", len(issues))
		}
	})

	t.Run("QueryPeerIssues rejects nonexistent path", func(t *testing.T) {
		_, err := dolt.QueryPeerIssues(ctx, "/nonexistent/path/to/repo", types.IssueFilter{})
		if err == nil {
			t.Fatal("expected error for nonexistent peer path")
		}
	})
}

// TestListRepoHydration tests HydrateFromPeerDolt for importing peer issues.
func TestListRepoHydration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Create peer store with issues
	peerDir := t.TempDir()
	peerDBPath := filepath.Join(peerDir, ".beads", "dolt")
	peerStore := newTestStoreWithPrefix(t, peerDBPath, "peer")

	for i, title := range []string{"Alpha task", "Beta bug"} {
		issueType := types.TypeTask
		if i == 1 {
			issueType = types.TypeBug
		}
		issue := &types.Issue{
			Title:     title,
			IssueType: issueType,
			Status:    types.StatusOpen,
			Priority:  2,
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		}
		if err := peerStore.CreateIssue(ctx, issue, "test"); err != nil {
			t.Fatalf("failed to create peer issue %q: %v", title, err)
		}
	}
	peerStore.Close()

	// Create local store
	localDir := t.TempDir()
	localDBPath := filepath.Join(localDir, ".beads", "dolt")
	localStore := newTestStoreWithPrefix(t, localDBPath, "local")

	t.Run("hydrate imports all peer issues", func(t *testing.T) {
		result, err := localStore.HydrateFromPeerDolt(ctx, peerDir)
		if err != nil {
			t.Fatalf("HydrateFromPeerDolt failed: %v", err)
		}
		if result.TotalPeerIssues != 2 {
			t.Errorf("expected 2 total peer issues, got %d", result.TotalPeerIssues)
		}
		if result.Imported != 2 {
			t.Errorf("expected 2 imported, got %d", result.Imported)
		}
		if result.Skipped != 0 {
			t.Errorf("expected 0 skipped, got %d", result.Skipped)
		}
	})

	t.Run("hydrate is idempotent", func(t *testing.T) {
		result, err := localStore.HydrateFromPeerDolt(ctx, peerDir)
		if err != nil {
			t.Fatalf("second HydrateFromPeerDolt failed: %v", err)
		}
		if result.Imported != 0 {
			t.Errorf("expected 0 imported on second run, got %d", result.Imported)
		}
		if result.Skipped != 2 {
			t.Errorf("expected 2 skipped on second run, got %d", result.Skipped)
		}
	})

	t.Run("hydrate rejects jsonl peer", func(t *testing.T) {
		jsonlDir := t.TempDir()
		beadsDir := filepath.Join(jsonlDir, ".beads")
		if err := os.MkdirAll(beadsDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "issues.jsonl"), []byte("{}"), 0644); err != nil {
			t.Fatal(err)
		}

		_, err := localStore.HydrateFromPeerDolt(ctx, jsonlDir)
		if err == nil {
			t.Fatal("expected error for JSONL peer")
		}
	})

	t.Run("hydrate rejects nonexistent peer", func(t *testing.T) {
		_, err := localStore.HydrateFromPeerDolt(ctx, "/nonexistent/path")
		if err == nil {
			t.Fatal("expected error for nonexistent peer")
		}
	})
}
