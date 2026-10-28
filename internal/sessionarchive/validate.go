package sessionarchive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// These schema fingerprints come from isolated official app-server
// initializations and are exercised by the native acceptance gate per version.
// They permit only the exact native triggers and prohibit additional triggers,
// views, virtual tables and arbitrary schema expressions
// in an untrusted archive before any staged database is opened for writing.
var nativeSchemaFingerprints = map[string]map[string]string{
	"0.163.0": {
		"state_5.sqlite":    "d0db110fce9a15a2f86a4553809bcb3ff50b5de13581f74f17d1146781a84276",
		"goals_1.sqlite":    "21a58c68564e332b540dc8714a738577b773a89ec813180a2948d74c0ab439dc",
		"queue_1.sqlite":    "a8d391824fc26b21cc7ea4a547b30a64ea42da392491de183ce3f7aee553b7b2",
		"memories_1.sqlite": "caae05f24d3219b5a2fccc6ed3ca15766ce11ae604de5b86cc939f54f4b7d674",
	},
	"0.165.0": {
		"state_5.sqlite":    "f86df5c3d56ea28d766afaba74bf6fd7dcd3665173fbe253aafdea84e8b7ba56",
		"goals_1.sqlite":    "21a58c68564e332b540dc8714a738577b773a89ec813180a2948d74c0ab439dc",
		"queue_1.sqlite":    "a8d391824fc26b21cc7ea4a547b30a64ea42da392491de183ce3f7aee553b7b2",
		"memories_1.sqlite": "caae05f24d3219b5a2fccc6ed3ca15766ce11ae604de5b86cc939f54f4b7d674",
	},
}

func validateNativeSnapshot(ctx context.Context, stage string, m Manifest) error {
	if err := supportedVersion(m.CodexVersion); err != nil {
		return err
	}
	schemas := nativeSchemaFingerprints[nativeVersion(m.CodexVersion)]
	all, err := scanRollouts(ctx, filepath.Join(stage, "native", "home"))
	if err != nil {
		return err
	}
	selected := map[string]bool{}
	for _, id := range m.ThreadIDs {
		selected[id] = true
	}
	required := map[string]int64{}
	seenOwner := map[string]bool{}
	for id, r := range all {
		if err := readRolloutMetadata(ctx, r); err != nil {
			return err
		}
		if selected[r.owner] {
			required[id] = -1
			seenOwner[r.owner] = true
		}
	}
	for id := range selected {
		if !seenOwner[id] {
			return fmt.Errorf("native archive thread has no owned rollout")
		}
	}
	for changed := true; changed; {
		changed = false
		for id := range required {
			r := all[id]
			if r.base == "" {
				continue
			}
			base := all[r.base]
			if base == nil {
				return fmt.Errorf("native archive is missing inherited history")
			}
			info, err := os.Stat(base.path)
			if err != nil {
				return err
			}
			if r.end > info.Size() {
				return fmt.Errorf("native inherited history cutoff exceeds source")
			}
			f, err := os.Open(base.path)
			if err != nil {
				return err
			}
			var tail [1]byte
			_, err = f.ReadAt(tail[:], r.end-1)
			f.Close()
			if err != nil || tail[0] != '\n' {
				return fmt.Errorf("native inherited history cutoff is not a record boundary")
			}
			if old, ok := required[r.base]; !ok || (old >= 0 && old < r.end) {
				required[r.base] = r.end
				changed = true
			}
		}
	}
	for id, r := range all {
		limit, ok := required[id]
		if !ok {
			return fmt.Errorf("archive contains unrelated native rollouts")
		}
		if limit >= 0 {
			info, err := os.Stat(r.path)
			if err != nil {
				return err
			}
			if info.Size() != limit {
				return fmt.Errorf("archive contains inherited history beyond its frozen prefix")
			}
		}
		chain := map[string]bool{}
		for current := id; current != ""; current = all[current].base {
			if chain[current] {
				return fmt.Errorf("native history lineage has a cycle")
			}
			chain[current] = true
		}
	}
	for _, filename := range nativeDBs {
		file := filepath.Join(stage, "native", "db", filename)
		if _, err := os.Lstat(file); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		db, err := openDB(file, true)
		if err != nil {
			return err
		}
		fingerprint, tables, err := nativeSchemaFingerprint(ctx, db)
		if err != nil {
			db.Close()
			return err
		}
		if fingerprint != schemas[filename] {
			db.Close()
			return fmt.Errorf("%w: unexpected native schema in %s (%s)", ErrUnsupported, filename, fingerprint)
		}
		if filename == "state_5.sqlite" {
			if err := checkAttachedThreads(ctx, db, m.ThreadIDs); err != nil {
				db.Close()
				return err
			}
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(m.ThreadIDs)), ",")
		args := make([]any, len(m.ThreadIDs))
		for i, id := range m.ThreadIDs {
			args[i] = id
		}
		for _, table := range tables {
			if table == "_sqlx_migrations" {
				continue
			}
			query := "SELECT count(*) FROM " + quote(table)
			var values []any
			switch table {
			case "threads":
				query += " WHERE id NOT IN (" + marks + ")"
				values = args
			case "thread_spawn_edges":
				query += " WHERE parent_thread_id NOT IN (" + marks + ") OR child_thread_id NOT IN (" + marks + ")"
				values = append(append([]any{}, args...), args...)
			case "projects":
				query += " WHERE id NOT IN (SELECT project_id FROM threads WHERE project_id IS NOT NULL)"
			case "project_roots":
				query += " WHERE project_id NOT IN (SELECT project_id FROM threads WHERE project_id IS NOT NULL)"
			case "thread_sections":
				query += " WHERE id NOT IN (SELECT thread_section_id FROM threads WHERE thread_section_id IS NOT NULL)"
			case "queued_items": // A queue is active work, never a resumable idle snapshot.
			default:
				cols, err := columnNames(ctx, db, table)
				if err != nil {
					db.Close()
					return err
				}
				for _, col := range cols {
					if col == "thread_id" {
						query += " WHERE thread_id NOT IN (" + marks + ")"
						values = args
						break
					}
				}
			}
			var n int
			if err := db.QueryRowContext(ctx, query, values...).Scan(&n); err != nil {
				db.Close()
				return err
			}
			if n != 0 {
				db.Close()
				return fmt.Errorf("archive contains unrelated or unsupported native rows in %s", table)
			}
		}
		if filename == "state_5.sqlite" {
			rows, err := db.QueryContext(ctx, "SELECT id,rollout_path FROM threads")
			if err != nil {
				db.Close()
				return err
			}
			for rows.Next() {
				var id, rolloutPath string
				if err := rows.Scan(&id, &rolloutPath); err != nil {
					rows.Close()
					db.Close()
					return err
				}
				if !filepath.IsAbs(rolloutPath) || !within(m.OriginalCodexHome, filepath.Clean(rolloutPath)) {
					rows.Close()
					db.Close()
					return fmt.Errorf("native metadata rollout path escapes its Codex home")
				}
				base := strings.TrimSuffix(filepath.Base(rolloutPath), ".zst")
				matched := false
				for _, r := range all {
					matched = matched || (r.owner == id && filepath.Base(r.path) == base)
				}
				if !matched {
					rows.Close()
					db.Close()
					return fmt.Errorf("native metadata references a missing rollout")
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				db.Close()
				return err
			}
		}
		db.Close()
	}
	closure, err := threadTree(ctx, filepath.Join(stage, "native", "db"), m.ThreadID)
	if err != nil {
		return err
	}
	if len(closure) != len(m.ThreadIDs) {
		return fmt.Errorf("native manifest thread tree differs from its spawn graph")
	}
	for _, id := range closure {
		if !selected[id] {
			return fmt.Errorf("native manifest includes an unrelated thread")
		}
	}
	return nil
}

func nativeSchemaFingerprint(ctx context.Context, db *sql.DB) (string, []string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type,name,tbl_name,coalesce(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name")
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	hash := sha256.New()
	var tables []string
	for rows.Next() {
		var kind, name, table, ddl string
		if err := rows.Scan(&kind, &name, &table, &ddl); err != nil {
			return "", nil, err
		}
		for _, field := range []string{kind, name, table, ddl} {
			hash.Write([]byte(field))
			hash.Write([]byte{0})
		}
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(hash.Sum(nil)), tables, nil
}

// Validate every existing target DB before publishing a single native file or
// row. Equal column names alone cannot prove a compatible schema or triggers.
func validateNativeRestoreTarget(ctx context.Context, o RestoreOptions, m Manifest) error {
	schemas := nativeSchemaFingerprints[nativeVersion(o.CodexVersion)]
	for _, name := range nativeDBs {
		path := filepath.Join(o.SQLiteHome, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		db, err := openDB(path, true)
		if err != nil {
			return err
		}
		fingerprint, _, err := nativeSchemaFingerprint(ctx, db)
		if err == nil && fingerprint != schemas[name] {
			err = fmt.Errorf("%w: target native schema in %s does not match Codex %s", ErrUnsupported, name, nativeVersion(o.CodexVersion))
		}
		if err == nil && name == "state_5.sqlite" {
			err = checkAttachedThreads(ctx, db, m.ThreadIDs)
		}
		db.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
