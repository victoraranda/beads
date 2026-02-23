package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/git"
	"github.com/steveyegge/beads/internal/storage/dolt"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// createTestSQLiteDB creates a SQLite database with the beads schema and test data.
func createTestSQLiteDB(t *testing.T, dbPath string) {
	t.Helper()

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("failed to create SQLite database: %v", err)
	}
	defer db.Close()

	// Create schema
	schema := `
		CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS issues (
			id TEXT PRIMARY KEY,
			content_hash TEXT DEFAULT '',
			title TEXT DEFAULT '',
			description TEXT DEFAULT '',
			design TEXT DEFAULT '',
			acceptance_criteria TEXT DEFAULT '',
			notes TEXT DEFAULT '',
			status TEXT DEFAULT 'open',
			priority INTEGER DEFAULT 2,
			issue_type TEXT DEFAULT 'task',
			assignee TEXT DEFAULT '',
			estimated_minutes INTEGER,
			created_at TEXT DEFAULT '',
			created_by TEXT DEFAULT '',
			owner TEXT DEFAULT '',
			updated_at TEXT DEFAULT '',
			closed_at TEXT,
			external_ref TEXT,
			compaction_level INTEGER DEFAULT 0,
			compacted_at TEXT DEFAULT '',
			compacted_at_commit TEXT,
			original_size INTEGER DEFAULT 0,
			sender TEXT DEFAULT '',
			ephemeral INTEGER DEFAULT 0,
			pinned INTEGER DEFAULT 0,
			is_template INTEGER DEFAULT 0,
			crystallizes INTEGER DEFAULT 0,
			mol_type TEXT DEFAULT '',
			work_type TEXT DEFAULT '',
			quality_score REAL,
			source_system TEXT DEFAULT '',
			source_repo TEXT DEFAULT '',
			close_reason TEXT DEFAULT '',
			event_kind TEXT DEFAULT '',
			actor TEXT DEFAULT '',
			target TEXT DEFAULT '',
			payload TEXT DEFAULT '',
			await_type TEXT DEFAULT '',
			await_id TEXT DEFAULT '',
			timeout_ns INTEGER DEFAULT 0,
			waiters TEXT DEFAULT '',
			hook_bead TEXT DEFAULT '',
			role_bead TEXT DEFAULT '',
			agent_state TEXT DEFAULT '',
			last_activity TEXT DEFAULT '',
			role_type TEXT DEFAULT '',
			rig TEXT DEFAULT '',
			due_at TEXT DEFAULT '',
			defer_until TEXT DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS labels (
			issue_id TEXT NOT NULL,
			label TEXT NOT NULL,
			PRIMARY KEY (issue_id, label)
		);
		CREATE TABLE IF NOT EXISTS dependencies (
			issue_id TEXT NOT NULL,
			depends_on_id TEXT NOT NULL,
			type TEXT DEFAULT '',
			created_by TEXT DEFAULT '',
			created_at TEXT DEFAULT '',
			PRIMARY KEY (issue_id, depends_on_id)
		);
		CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			issue_id TEXT NOT NULL,
			event_type TEXT DEFAULT '',
			actor TEXT DEFAULT '',
			old_value TEXT,
			new_value TEXT,
			comment TEXT,
			created_at TEXT DEFAULT ''
		);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("failed to create schema: %v", err)
	}

	// Insert config
	if _, err := db.Exec(`INSERT INTO config (key, value) VALUES ('issue_prefix', 'test')`); err != nil {
		t.Fatalf("failed to insert config: %v", err)
	}

	// Insert test issues
	if _, err := db.Exec(`
		INSERT INTO issues (id, title, status, priority, issue_type, created_at, updated_at, created_by)
		VALUES ('test-abc1', 'First test issue', 'open', 1, 'bug', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'tester')
	`); err != nil {
		t.Fatalf("failed to insert issue 1: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO issues (id, title, status, priority, issue_type, created_at, updated_at, created_by)
		VALUES ('test-def2', 'Second test issue', 'closed', 2, 'task', '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z', 'tester')
	`); err != nil {
		t.Fatalf("failed to insert issue 2: %v", err)
	}

	// Insert labels
	if _, err := db.Exec(`INSERT INTO labels (issue_id, label) VALUES ('test-abc1', 'critical')`); err != nil {
		t.Fatalf("failed to insert label: %v", err)
	}

	// Insert dependency
	if _, err := db.Exec(`
		INSERT INTO dependencies (issue_id, depends_on_id, type, created_by, created_at)
		VALUES ('test-def2', 'test-abc1', 'blocks', 'tester', '2026-01-01T00:00:00Z')
	`); err != nil {
		t.Fatalf("failed to insert dependency: %v", err)
	}

	// Insert event
	if _, err := db.Exec(`
		INSERT INTO events (issue_id, event_type, actor, comment, created_at)
		VALUES ('test-abc1', 'comment', 'tester', 'Test comment', '2026-01-01T12:00:00Z')
	`); err != nil {
		t.Fatalf("failed to insert event: %v", err)
	}
}

func TestAutoMigrateSQLiteDetection(t *testing.T) {
	// Test amFindSQLiteDB
	dir := t.TempDir()

	// No .db files → empty result
	if got := amFindSQLiteDB(dir); got != "" {
		t.Errorf("amFindSQLiteDB(empty dir) = %q, want empty", got)
	}

	// Create beads.db → should find it
	dbPath := filepath.Join(dir, "beads.db")
	if err := os.WriteFile(dbPath, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := amFindSQLiteDB(dir); got != dbPath {
		t.Errorf("amFindSQLiteDB(with beads.db) = %q, want %q", got, dbPath)
	}

	// With dolt directory → checkAndAutoMigrateSQLite should return false
	doltDir := filepath.Join(dir, "dolt")
	if err := os.MkdirAll(doltDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Temporarily set BEADS_DIR
	t.Setenv("BEADS_DIR", dir)
	beads.ResetCaches()
	defer func() { beads.ResetCaches() }()
	if got := checkAndAutoMigrateSQLite(); got {
		t.Error("checkAndAutoMigrateSQLite returned true when dolt/ exists")
	}
}

func TestAutoMigrateSQLiteSkipMigratedFiles(t *testing.T) {
	dir := t.TempDir()

	// Create beads.db.migrated (but no beads.db) → should not find anything
	migratedPath := filepath.Join(dir, "beads.db.migrated")
	if err := os.WriteFile(migratedPath, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := amFindSQLiteDB(dir); got != "" {
		t.Errorf("amFindSQLiteDB should skip .migrated files, got %q", got)
	}

	// Create backup file → should not find it
	backupPath := filepath.Join(dir, "beads.backup.db")
	if err := os.WriteFile(backupPath, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := amFindSQLiteDB(dir); got != "" {
		t.Errorf("amFindSQLiteDB should skip backup files, got %q", got)
	}
}

func TestAutoMigrateExtraction(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "beads.db")
	createTestSQLiteDB(t, dbPath)

	ctx := context.Background()
	data, err := amExtractFromSQLite(ctx, dbPath)
	if err != nil {
		t.Fatalf("amExtractFromSQLite failed: %v", err)
	}

	// Check prefix
	if data.prefix != "test" {
		t.Errorf("prefix = %q, want %q", data.prefix, "test")
	}

	// Check issues
	if len(data.issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(data.issues))
	}

	// Verify first issue
	found := false
	for _, issue := range data.issues {
		if issue.ID == "test-abc1" {
			found = true
			if issue.Title != "First test issue" {
				t.Errorf("issue title = %q, want %q", issue.Title, "First test issue")
			}
			if string(issue.Status) != "open" {
				t.Errorf("issue status = %q, want %q", issue.Status, "open")
			}
			if len(issue.Labels) != 1 || issue.Labels[0] != "critical" {
				t.Errorf("issue labels = %v, want [critical]", issue.Labels)
			}
		}
	}
	if !found {
		t.Error("issue test-abc1 not found in extracted data")
	}

	// Check config
	if data.config["issue_prefix"] != "test" {
		t.Errorf("config[issue_prefix] = %q, want %q", data.config["issue_prefix"], "test")
	}

	// Check labels map
	if len(data.labelsMap["test-abc1"]) != 1 {
		t.Errorf("labelsMap[test-abc1] = %v, want 1 label", data.labelsMap["test-abc1"])
	}

	// Check events
	if len(data.eventsMap["test-abc1"]) != 1 {
		t.Errorf("eventsMap[test-abc1] has %d events, want 1", len(data.eventsMap["test-abc1"]))
	}
}

func TestAutoMigrateFullPipeline(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create SQLite database with test data
	dbPath := filepath.Join(beadsDir, "beads.db")
	createTestSQLiteDB(t, dbPath)

	// Perform migration
	ctx := context.Background()
	err := amPerformMigration(ctx, beadsDir, dbPath)
	if err != nil {
		// Skip if Dolt server not available
		if isNoDoltServer(err) {
			t.Skipf("skipping: Dolt server not available: %v", err)
		}
		t.Fatalf("amPerformMigration failed: %v", err)
	}

	// Verify: SQLite file renamed
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Error("beads.db should have been renamed after migration")
	}
	migratedPath := dbPath + ".migrated"
	if _, err := os.Stat(migratedPath); os.IsNotExist(err) {
		t.Error("beads.db.migrated should exist after migration")
	}

	// Verify: dolt directory created
	doltPath := filepath.Join(beadsDir, "dolt")
	if _, err := os.Stat(doltPath); os.IsNotExist(err) {
		t.Error("dolt/ directory should exist after migration")
	}

	// Verify: metadata.json updated
	cfg, err := configfile.Load(beadsDir)
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}
	if cfg.Backend != configfile.BackendDolt {
		t.Errorf("backend = %q, want %q", cfg.Backend, configfile.BackendDolt)
	}
	if cfg.Database != "dolt" {
		t.Errorf("database = %q, want %q", cfg.Database, "dolt")
	}
	if cfg.DoltDatabase != "beads_test" {
		t.Errorf("dolt_database = %q, want %q", cfg.DoltDatabase, "beads_test")
	}

	// Verify: issues imported to Dolt
	store, err := dolt.NewFromConfig(ctx, beadsDir)
	if err != nil {
		t.Fatalf("failed to open Dolt store: %v", err)
	}
	defer func() { _ = store.Close() }()

	issue, err := store.GetIssue(ctx, "test-abc1")
	if err != nil {
		t.Fatalf("failed to get issue test-abc1 from Dolt: %v", err)
	}
	if issue.Title != "First test issue" {
		t.Errorf("Dolt issue title = %q, want %q", issue.Title, "First test issue")
	}

	issue2, err := store.GetIssue(ctx, "test-def2")
	if err != nil {
		t.Fatalf("failed to get issue test-def2 from Dolt: %v", err)
	}
	if issue2.Title != "Second test issue" {
		t.Errorf("Dolt issue title = %q, want %q", issue2.Title, "Second test issue")
	}
}

func TestAutoMigratePartialRetry(t *testing.T) {
	dir := t.TempDir()

	// Create both beads.db and beads.db.migrated (simulates partial migration)
	dbPath := filepath.Join(dir, "beads.db")
	migratedPath := filepath.Join(dir, "beads.db.migrated")
	if err := os.WriteFile(dbPath, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(migratedPath, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}

	// Set BEADS_DIR and call checkAndAutoMigrateSQLite
	// The .migrated file should be removed as part of retry detection
	t.Setenv("BEADS_DIR", dir)
	beads.ResetCaches()
	git.ResetCaches()
	defer func() {
		beads.ResetCaches()
		git.ResetCaches()
	}()

	// We can't fully run the migration (no valid SQLite + no Dolt server),
	// but we can verify the retry detection by checking if .migrated was removed
	_ = checkAndAutoMigrateSQLite() // Will fail on extraction, but should remove .migrated first

	// The .migrated file should have been removed before extraction attempt
	if _, err := os.Stat(migratedPath); !os.IsNotExist(err) {
		t.Error("beads.db.migrated should be removed during partial migration retry")
	}
}

func TestAutoMigrateNullTimeParser(t *testing.T) {
	tests := []struct {
		input string
		isNil bool
	}{
		{"", true},
		{"2026-01-01T00:00:00Z", false},
		{"2026-01-01T00:00:00.123456Z", false},
		{"2026-01-01 00:00:00", false},
		{"not-a-time", true},
	}
	for _, tt := range tests {
		result := amParseNullTime(tt.input)
		if tt.isNil && result != nil {
			t.Errorf("amParseNullTime(%q) = %v, want nil", tt.input, result)
		}
		if !tt.isNil && result == nil {
			t.Errorf("amParseNullTime(%q) = nil, want non-nil", tt.input)
		}
	}
}

// isNoDoltServer checks if an error indicates the Dolt server is not available.
func isNoDoltServer(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") ||
		strings.Contains(s, "dial tcp") ||
		strings.Contains(s, "failed to create Dolt database")
}
