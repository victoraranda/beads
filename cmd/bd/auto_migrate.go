package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/beads/internal/beads"
	"github.com/steveyegge/beads/internal/config"
	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/debug"
	"github.com/steveyegge/beads/internal/storage/dolt"
	"github.com/steveyegge/beads/internal/types"

	_ "github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/embed"
)

// autoMigrateData holds data extracted from SQLite for auto-migration.
type autoMigrateData struct {
	issues    []*types.Issue
	labelsMap map[string][]string
	depsMap   map[string][]*types.Dependency
	eventsMap map[string][]*types.Event
	config    map[string]string
	prefix    string
}

// checkAndAutoMigrateSQLite detects a legacy SQLite beads.db and automatically
// migrates it to Dolt. This runs at startup before database path discovery.
//
// Detection: beads.db (or any .db file) exists in .beads/ AND no dolt/ directory.
//
// Edge cases:
//   - Corrupted SQLite: warns and skips (user loses data but can retry manually)
//   - Partial migration (beads.db.migrated + beads.db): removes .migrated and retries
//   - Daemon with stale SQLite: warns and proceeds (best effort)
//
// Returns true if migration was performed (caller should retry database discovery).
func checkAndAutoMigrateSQLite() bool {
	beadsDir := beads.FindBeadsDir()
	if beadsDir == "" {
		return false
	}

	// Look for SQLite .db file
	sqlitePath := amFindSQLiteDB(beadsDir)
	if sqlitePath == "" {
		return false
	}

	// Check that Dolt directory does NOT exist — if it does, no migration needed
	doltPath := filepath.Join(beadsDir, "dolt")
	if info, err := os.Stat(doltPath); err == nil && info.IsDir() {
		return false
	}

	// Handle partial migration: .migrated + original .db = retry
	migratedPath := sqlitePath + ".migrated"
	if _, err := os.Stat(migratedPath); err == nil {
		debug.Logf("auto-migrate: partial migration detected (both %s and %s exist), retrying",
			filepath.Base(sqlitePath), filepath.Base(migratedPath))
		_ = os.Remove(migratedPath)
	}

	// Perform migration
	fmt.Fprintf(os.Stderr, "Detected legacy SQLite database, migrating to Dolt...\n")

	ctx := context.Background()
	if rootCtx != nil && rootCtx.Err() == nil {
		ctx = rootCtx
	}

	if err := amPerformMigration(ctx, beadsDir, sqlitePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-migration failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "You can manually migrate with: bd migrate --to-dolt\n")
		return false
	}

	return true
}

// amFindSQLiteDB looks for a legacy SQLite .db file in the beads directory.
// Returns the path to the first .db file found, or empty string if none.
// Skips backup and already-migrated files.
func amFindSQLiteDB(beadsDir string) string {
	// Check common names first
	for _, name := range []string{"beads.db", "issues.db"} {
		p := filepath.Join(beadsDir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	// Scan for any .db file (excluding backups and migrated files)
	entries, err := os.ReadDir(beadsDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".db") &&
			!strings.Contains(name, "backup") &&
			!strings.Contains(name, "migrated") {
			return filepath.Join(beadsDir, name)
		}
	}
	return ""
}

// amPerformMigration executes the SQLite → Dolt migration:
// 1. Extract all data from SQLite (read-only)
// 2. Create Dolt database and import data
// 3. Update metadata.json
// 4. Rename beads.db → beads.db.migrated
func amPerformMigration(ctx context.Context, beadsDir, sqlitePath string) error {
	// Step 1: Extract from SQLite
	data, err := amExtractFromSQLite(ctx, sqlitePath)
	if err != nil {
		return fmt.Errorf("failed to read SQLite database: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  Found %d issues in SQLite\n", len(data.issues))

	// Step 2: Create Dolt database
	doltPath := filepath.Join(beadsDir, "dolt")
	dbName := "beads"
	if data.prefix != "" {
		dbName = "beads_" + data.prefix
	}

	if err := os.MkdirAll(doltPath, 0750); err != nil {
		return fmt.Errorf("failed to create dolt directory: %w", err)
	}

	store, err := dolt.New(ctx, &dolt.Config{Path: doltPath, Database: dbName})
	if err != nil {
		_ = os.RemoveAll(doltPath)
		return fmt.Errorf("failed to create Dolt database: %w", err)
	}

	// Import data (cleanup on failure)
	imported, skipped, importErr := amImportToDolt(ctx, store, data)
	if importErr != nil {
		_ = store.Close()
		_ = os.RemoveAll(doltPath)
		return fmt.Errorf("import failed (partial Dolt directory cleaned up): %w", importErr)
	}

	// Set sync mode
	if err := store.SetConfig(ctx, "sync.mode", "dolt-native"); err != nil {
		debug.Logf("auto-migrate: failed to set sync.mode: %v", err)
	}

	// Commit the migration
	commitMsg := fmt.Sprintf("auto-migrate: imported %d issues from SQLite", imported)
	if err := store.Commit(ctx, commitMsg); err != nil {
		debug.Logf("auto-migrate: failed to create Dolt commit: %v", err)
	}

	_ = store.Close()

	fmt.Fprintf(os.Stderr, "  Imported %d issues (%d skipped)\n", imported, skipped)

	// Step 3: Update metadata.json
	cfg, err := configfile.Load(beadsDir)
	if err != nil || cfg == nil {
		cfg = configfile.DefaultConfig()
	}
	cfg.Backend = configfile.BackendDolt
	cfg.Database = "dolt"
	cfg.DoltDatabase = dbName
	cfg.DoltMode = configfile.DoltModeServer
	cfg.DoltServerPort = configfile.DefaultDoltServerPort
	if err := cfg.Save(beadsDir); err != nil {
		return fmt.Errorf("failed to update metadata.json: %w", err)
	}

	// Update config.yaml sync mode (best-effort)
	_ = config.SaveConfigValue("sync.mode", string(config.SyncModeDoltNative), beadsDir)

	// Step 4: Rename SQLite file
	migratedPath := sqlitePath + ".migrated"
	if err := os.Rename(sqlitePath, migratedPath); err != nil {
		return fmt.Errorf("failed to rename SQLite file: %w", err)
	}

	fmt.Fprintf(os.Stderr, "  Renamed %s → %s\n", filepath.Base(sqlitePath), filepath.Base(migratedPath))
	fmt.Fprintf(os.Stderr, "  Migration complete.\n")

	return nil
}

// amExtractFromSQLite extracts all data from a SQLite database in read-only mode.
// This is a self-contained reader that handles corrupted databases gracefully.
func amExtractFromSQLite(ctx context.Context, dbPath string) (*autoMigrateData, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("failed to open SQLite database: %w", err)
	}
	defer db.Close()

	// Verify the database is accessible
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("SQLite database appears corrupted: %w", err)
	}

	// Get prefix from config table
	prefix := ""
	_ = db.QueryRowContext(ctx, "SELECT value FROM config WHERE key = 'issue_prefix'").Scan(&prefix)

	// Get all config
	cfgMap := make(map[string]string)
	configRows, err := db.QueryContext(ctx, "SELECT key, value FROM config")
	if err == nil {
		defer configRows.Close()
		for configRows.Next() {
			var k, v string
			if err := configRows.Scan(&k, &v); err == nil {
				cfgMap[k] = v
			}
		}
	}

	// Get all issues
	issues, err := amExtractIssues(ctx, db)
	if err != nil {
		return nil, err
	}

	// Get labels
	labelsMap := make(map[string][]string)
	labelRows, err := db.QueryContext(ctx, "SELECT issue_id, label FROM labels")
	if err == nil {
		defer labelRows.Close()
		for labelRows.Next() {
			var issueID, label string
			if err := labelRows.Scan(&issueID, &label); err == nil {
				labelsMap[issueID] = append(labelsMap[issueID], label)
			}
		}
	}

	// Get dependencies
	depsMap := make(map[string][]*types.Dependency)
	depRows, err := db.QueryContext(ctx, "SELECT issue_id, depends_on_id, COALESCE(type,''), COALESCE(created_by,''), COALESCE(created_at,'') FROM dependencies")
	if err == nil {
		defer depRows.Close()
		for depRows.Next() {
			var dep types.Dependency
			if err := depRows.Scan(&dep.IssueID, &dep.DependsOnID, &dep.Type, &dep.CreatedBy, &dep.CreatedAt); err == nil {
				depsMap[dep.IssueID] = append(depsMap[dep.IssueID], &dep)
			}
		}
	}

	// Get events (history/comments)
	eventsMap := make(map[string][]*types.Event)
	eventRows, err := db.QueryContext(ctx, "SELECT issue_id, COALESCE(event_type,''), COALESCE(actor,''), old_value, new_value, comment, COALESCE(created_at,'') FROM events")
	if err == nil {
		defer eventRows.Close()
		for eventRows.Next() {
			var issueID string
			var event types.Event
			var oldVal, newVal, comment sql.NullString
			if err := eventRows.Scan(&issueID, &event.EventType, &event.Actor, &oldVal, &newVal, &comment, &event.CreatedAt); err == nil {
				if oldVal.Valid {
					event.OldValue = &oldVal.String
				}
				if newVal.Valid {
					event.NewValue = &newVal.String
				}
				if comment.Valid {
					event.Comment = &comment.String
				}
				eventsMap[issueID] = append(eventsMap[issueID], &event)
			}
		}
	}

	// Assign labels and dependencies to issues
	for _, issue := range issues {
		if labels, ok := labelsMap[issue.ID]; ok {
			issue.Labels = labels
		}
		if deps, ok := depsMap[issue.ID]; ok {
			issue.Dependencies = deps
		}
	}

	return &autoMigrateData{
		issues:    issues,
		labelsMap: labelsMap,
		depsMap:   depsMap,
		eventsMap: eventsMap,
		config:    cfgMap,
		prefix:    prefix,
	}, nil
}

// amExtractIssues reads all issues from the SQLite database.
func amExtractIssues(ctx context.Context, db *sql.DB) ([]*types.Issue, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(content_hash,''), COALESCE(title,''), COALESCE(description,''),
			COALESCE(design,''), COALESCE(acceptance_criteria,''), COALESCE(notes,''),
			COALESCE(status,''), COALESCE(priority,0), COALESCE(issue_type,''),
			COALESCE(assignee,''), estimated_minutes,
			COALESCE(created_at,''), COALESCE(created_by,''), COALESCE(owner,''),
			COALESCE(updated_at,''), COALESCE(closed_at,''), external_ref,
			COALESCE(compaction_level,0), COALESCE(compacted_at,''), compacted_at_commit,
			COALESCE(original_size,0),
			COALESCE(sender,''), COALESCE(ephemeral,0), COALESCE(pinned,0),
			COALESCE(is_template,0), COALESCE(crystallizes,0),
			COALESCE(mol_type,''), COALESCE(work_type,''), quality_score,
			COALESCE(source_system,''), COALESCE(source_repo,''), COALESCE(close_reason,''),
			COALESCE(event_kind,''), COALESCE(actor,''), COALESCE(target,''), COALESCE(payload,''),
			COALESCE(await_type,''), COALESCE(await_id,''), COALESCE(timeout_ns,0), COALESCE(waiters,''),
			COALESCE(hook_bead,''), COALESCE(role_bead,''), COALESCE(agent_state,''),
			COALESCE(last_activity,''), COALESCE(role_type,''), COALESCE(rig,''),
			COALESCE(due_at,''), COALESCE(defer_until,'')
		FROM issues`)
	if err != nil {
		return nil, fmt.Errorf("failed to query issues: %w", err)
	}
	defer rows.Close()

	var issues []*types.Issue
	for rows.Next() {
		var issue types.Issue
		var estMin sql.NullInt64
		var extRef, compactCommit sql.NullString
		var qualScore sql.NullFloat64
		var timeoutNs int64
		var waitersJSON string
		var closedAt, compactedAt, lastActivity, dueAt, deferUntil sql.NullString
		if err := rows.Scan(
			&issue.ID, &issue.ContentHash, &issue.Title, &issue.Description,
			&issue.Design, &issue.AcceptanceCriteria, &issue.Notes,
			&issue.Status, &issue.Priority, &issue.IssueType,
			&issue.Assignee, &estMin,
			&issue.CreatedAt, &issue.CreatedBy, &issue.Owner,
			&issue.UpdatedAt, &closedAt, &extRef,
			&issue.CompactionLevel, &compactedAt, &compactCommit,
			&issue.OriginalSize,
			&issue.Sender, &issue.Ephemeral, &issue.Pinned,
			&issue.IsTemplate, &issue.Crystallizes,
			&issue.MolType, &issue.WorkType, &qualScore,
			&issue.SourceSystem, &issue.SourceRepo, &issue.CloseReason,
			&issue.EventKind, &issue.Actor, &issue.Target, &issue.Payload,
			&issue.AwaitType, &issue.AwaitID, &timeoutNs, &waitersJSON,
			&issue.HookBead, &issue.RoleBead, &issue.AgentState,
			&lastActivity, &issue.RoleType, &issue.Rig,
			&dueAt, &deferUntil,
		); err != nil {
			return nil, fmt.Errorf("failed to scan issue: %w", err)
		}
		if estMin.Valid {
			v := int(estMin.Int64)
			issue.EstimatedMinutes = &v
		}
		if extRef.Valid {
			issue.ExternalRef = &extRef.String
		}
		if compactCommit.Valid {
			issue.CompactedAtCommit = &compactCommit.String
		}
		if qualScore.Valid {
			v := float32(qualScore.Float64)
			issue.QualityScore = &v
		}
		issue.ClosedAt = amParseNullTime(closedAt.String)
		issue.CompactedAt = amParseNullTime(compactedAt.String)
		issue.LastActivity = amParseNullTime(lastActivity.String)
		issue.DueAt = amParseNullTime(dueAt.String)
		issue.DeferUntil = amParseNullTime(deferUntil.String)
		issue.Timeout = time.Duration(timeoutNs)
		if waitersJSON != "" {
			_ = json.Unmarshal([]byte(waitersJSON), &issue.Waiters)
		}
		issues = append(issues, &issue)
	}

	return issues, nil
}

// amImportToDolt imports all data to a Dolt store, returning (imported, skipped, error).
func amImportToDolt(ctx context.Context, store *dolt.DoltStore, data *autoMigrateData) (int, int, error) {
	// Set config values
	for key, value := range data.config {
		if err := store.SetConfig(ctx, key, value); err != nil {
			return 0, 0, fmt.Errorf("failed to set config %s: %w", key, err)
		}
	}

	tx, err := store.UnderlyingDB().BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	imported := 0
	skipped := 0
	seenIDs := make(map[string]bool)

	for _, issue := range data.issues {
		if seenIDs[issue.ID] {
			skipped++
			continue
		}
		seenIDs[issue.ID] = true

		if issue.ContentHash == "" {
			issue.ContentHash = issue.ComputeContentHash()
		}

		_, err := tx.ExecContext(ctx, `
			INSERT INTO issues (
				id, content_hash, title, description, design, acceptance_criteria, notes,
				status, priority, issue_type, assignee, estimated_minutes,
				created_at, created_by, owner, updated_at, closed_at, external_ref,
				compaction_level, compacted_at, compacted_at_commit, original_size,
				sender, ephemeral, pinned, is_template, crystallizes,
				mol_type, work_type, quality_score, source_system, source_repo, close_reason,
				event_kind, actor, target, payload,
				await_type, await_id, timeout_ns, waiters,
				hook_bead, role_bead, agent_state, last_activity, role_type, rig,
				due_at, defer_until
			) VALUES (
				?, ?, ?, ?, ?, ?, ?,
				?, ?, ?, ?, ?,
				?, ?, ?, ?, ?, ?,
				?, ?, ?, ?,
				?, ?, ?, ?, ?,
				?, ?, ?, ?, ?, ?,
				?, ?, ?, ?,
				?, ?, ?, ?,
				?, ?, ?, ?, ?, ?,
				?, ?
			)
		`,
			issue.ID, issue.ContentHash, issue.Title, issue.Description, issue.Design, issue.AcceptanceCriteria, issue.Notes,
			issue.Status, issue.Priority, issue.IssueType, amNullStr(issue.Assignee), amNullIntPtr(issue.EstimatedMinutes),
			issue.CreatedAt, issue.CreatedBy, issue.Owner, issue.UpdatedAt, issue.ClosedAt, amNullStrPtr(issue.ExternalRef),
			issue.CompactionLevel, issue.CompactedAt, amNullStrPtr(issue.CompactedAtCommit), amNullInt(issue.OriginalSize),
			issue.Sender, issue.Ephemeral, issue.Pinned, issue.IsTemplate, issue.Crystallizes,
			issue.MolType, issue.WorkType, amNullFloat32Ptr(issue.QualityScore), issue.SourceSystem, issue.SourceRepo, issue.CloseReason,
			issue.EventKind, issue.Actor, issue.Target, issue.Payload,
			issue.AwaitType, issue.AwaitID, issue.Timeout.Nanoseconds(), amFormatJSONArray(issue.Waiters),
			issue.HookBead, issue.RoleBead, issue.AgentState, issue.LastActivity, issue.RoleType, issue.Rig,
			issue.DueAt, issue.DeferUntil,
		)
		if err != nil {
			if strings.Contains(err.Error(), "Duplicate entry") ||
				strings.Contains(err.Error(), "UNIQUE constraint") ||
				strings.Contains(err.Error(), "duplicate primary key") {
				skipped++
				continue
			}
			return imported, skipped, fmt.Errorf("failed to insert issue %s: %w", issue.ID, err)
		}

		// Insert labels
		for _, label := range issue.Labels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO labels (issue_id, label) VALUES (?, ?)`, issue.ID, label); err != nil {
				debug.Logf("auto-migrate: failed to insert label %q for issue %s: %v", label, issue.ID, err)
			}
		}

		imported++
	}

	// Import dependencies
	for _, issue := range data.issues {
		for _, dep := range issue.Dependencies {
			var exists int
			if err := tx.QueryRowContext(ctx, "SELECT 1 FROM issues WHERE id = ?", dep.DependsOnID).Scan(&exists); err != nil {
				continue // Skip dependencies with missing target
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO dependencies (issue_id, depends_on_id, type, created_by, created_at)
				VALUES (?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE type = type
			`, dep.IssueID, dep.DependsOnID, dep.Type, dep.CreatedBy, dep.CreatedAt); err != nil {
				debug.Logf("auto-migrate: failed to insert dependency %s -> %s: %v", dep.IssueID, dep.DependsOnID, err)
			}
		}
	}

	// Import events
	eventCount := 0
	for issueID, events := range data.eventsMap {
		for _, event := range events {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO events (issue_id, event_type, actor, old_value, new_value, comment, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
			`, issueID, event.EventType, event.Actor,
				amNullStrPtr(event.OldValue), amNullStrPtr(event.NewValue),
				amNullStrPtr(event.Comment), event.CreatedAt)
			if err == nil {
				eventCount++
			}
		}
	}
	debug.Logf("auto-migrate: imported %d events", eventCount)

	if err := tx.Commit(); err != nil {
		return imported, skipped, fmt.Errorf("failed to commit: %w", err)
	}

	return imported, skipped, nil
}

// amParseNullTime parses a time string into *time.Time. Returns nil for empty strings.
func amParseNullTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// Nullable helpers for SQL parameters (prefixed to avoid CGO build conflicts).

func amNullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func amNullStrPtr(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

func amNullIntPtr(i *int) interface{} {
	if i == nil {
		return nil
	}
	return *i
}

func amNullInt(i int) interface{} {
	if i == 0 {
		return nil
	}
	return i
}

func amNullFloat32Ptr(f *float32) interface{} {
	if f == nil {
		return nil
	}
	return *f
}

func amFormatJSONArray(arr []string) string {
	if len(arr) == 0 {
		return ""
	}
	data, err := json.Marshal(arr)
	if err != nil {
		return ""
	}
	return string(data)
}
