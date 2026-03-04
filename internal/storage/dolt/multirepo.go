//go:build cgo

package dolt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/types"
)

// PeerBackend represents the detected storage backend of a peer repository.
type PeerBackend string

const (
	// PeerBackendDolt indicates the peer uses a Dolt database.
	PeerBackendDolt PeerBackend = "dolt"
	// PeerBackendJSONL indicates the peer uses JSONL file storage.
	PeerBackendJSONL PeerBackend = "jsonl"
	// PeerBackendUnknown indicates the peer backend could not be determined.
	PeerBackendUnknown PeerBackend = "unknown"
)

// DetectPeerBackend determines the storage backend type of a peer repository.
// peerRepoPath is the root directory of the peer repository (containing .beads/).
// Returns the detected backend type or an error if the path is invalid.
func DetectPeerBackend(peerRepoPath string) (PeerBackend, error) {
	beadsDir := filepath.Join(peerRepoPath, ".beads")
	info, err := os.Stat(beadsDir)
	if err != nil {
		return PeerBackendUnknown, fmt.Errorf("no .beads directory at %s: %w", peerRepoPath, err)
	}
	if !info.IsDir() {
		return PeerBackendUnknown, fmt.Errorf(".beads at %s is not a directory", peerRepoPath)
	}

	// Check for Dolt database directory
	doltDir := filepath.Join(beadsDir, "dolt")
	if info, err := os.Stat(doltDir); err == nil && info.IsDir() {
		return PeerBackendDolt, nil
	}

	// Check for JSONL file
	jsonlPath := filepath.Join(beadsDir, "issues.jsonl")
	if _, err := os.Stat(jsonlPath); err == nil {
		return PeerBackendJSONL, nil
	}

	// Check metadata.json for backend hint
	cfg, err := configfile.Load(beadsDir)
	if err == nil && cfg != nil {
		if cfg.GetBackend() == configfile.BackendDolt {
			return PeerBackendDolt, nil
		}
	}

	return PeerBackendUnknown, fmt.Errorf("could not determine backend for peer at %s", peerRepoPath)
}

// OpenPeerStore opens a read-only DoltStore for a peer repository.
// peerRepoPath is the root directory of the peer repository (containing .beads/).
// Uses AutoStart to ensure a Dolt server is running for the peer database.
// The server is managed by doltserver.EnsureRunning and persists across queries
// (it is not started/stopped per call).
// The caller must close the returned store when done.
func OpenPeerStore(ctx context.Context, peerRepoPath string) (*DoltStore, error) {
	beadsDir := filepath.Join(peerRepoPath, ".beads")

	// Verify the beads directory exists
	if _, err := os.Stat(beadsDir); err != nil {
		return nil, fmt.Errorf("peer repository not found at %s: %w", peerRepoPath, err)
	}

	// Open in read-only mode with AutoStart to ensure a server is running.
	// Each beads directory gets its own Dolt server on a derived port.
	peerStore, err := NewFromConfigWithOptions(ctx, beadsDir, &Config{
		ReadOnly:  true,
		AutoStart: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open peer database at %s: %w", peerRepoPath, err)
	}

	return peerStore, nil
}

// QueryPeerIssues queries issues from a peer Dolt database with the given filter.
// Results are tagged with the SourceRepo field set to peerRepoPath.
// This is a read-only operation that connects to the peer's Dolt server.
func QueryPeerIssues(ctx context.Context, peerRepoPath string, filter types.IssueFilter) ([]*types.Issue, error) {
	peerStore, err := OpenPeerStore(ctx, peerRepoPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = peerStore.Close() }()

	issues, err := peerStore.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, fmt.Errorf("failed to query peer issues at %s: %w", peerRepoPath, err)
	}

	// Tag all results with the source repository path
	for _, issue := range issues {
		issue.SourceRepo = peerRepoPath
	}

	return issues, nil
}
