package sessionarchive

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
)

func selectedIDs(m Manifest) map[string]bool {
	ids := map[string]bool{}
	for _, id := range m.ThreadIDs {
		ids[id] = true
	}
	return ids
}

// Fork ancestors can also belong to unrelated live threads. A retained-thread
// repair gives frozen ancestor copies deterministic, private rollout IDs so no
// unrelated native data is ever changed.
func repairAliases(ctx context.Context, m Manifest, before string) (map[string]string, error) {
	all, err := scanRollouts(ctx, filepath.Join(before, "native", "home"))
	if err != nil {
		return nil, err
	}
	selected := selectedIDs(m)
	aliases := map[string]string{}
	for id, r := range all {
		if !selected[r.owner] {
			aliases[id] = uuid.NewSHA1(uuid.NameSpaceURL, []byte("telegramgw-archive-repair:"+m.ArchiveID+":"+id)).String()
		}
	}
	return aliases, nil
}

func repairRetainedNative(ctx context.Context, o RestoreOptions, m Manifest, before, after string) error {
	old, err := scanRollouts(ctx, filepath.Join(before, "native", "home"))
	if err != nil {
		return err
	}
	desired, err := scanRollouts(ctx, filepath.Join(after, "native", "home"))
	if err != nil {
		return err
	}
	live, err := scanRollouts(ctx, o.CodexHome)
	if err != nil {
		return err
	}
	selected := selectedIDs(m)
	for id, r := range live {
		if selected[r.owner] {
			if old[id] == nil || !within(filepath.Join(o.CodexHome, "archived_sessions"), r.path) {
				return fmt.Errorf("%w: native repair requires unchanged archived thread generations", ErrConflict)
			}
			if rolloutsEqual(ctx, old[id], r, false) != nil && rolloutsEqual(ctx, desired[id], r, false) != nil {
				return fmt.Errorf("%w: retained native history changed during restore", ErrConflict)
			}
		}
	}
	for id, r := range old {
		if selected[r.owner] && live[id] == nil {
			return fmt.Errorf("%w: retained native history vanished during restore", ErrConflict)
		}
	}
	// Validate every row against either its exact preimage or its exact desired
	// value before publishing any replacement. Lifecycle fields stay native-owned.
	for _, name := range nativeDBs {
		src := filepath.Join(before, "native", "db", name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		if err := repairDB(ctx, src, filepath.Join(after, "native", "db", name), filepath.Join(o.SQLiteHome, name), false, m.ThreadIDs); err != nil {
			return err
		}
	}
	if err := publishManagedAttachments(ctx, o, m, after); err != nil {
		return err
	}
	var ids []string
	for id := range desired {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := desired[id]
		if selected[r.owner] {
			if err := replaceNativeRollout(ctx, old[id], r, live[id]); err != nil {
				return err
			}
		} else {
			target := filepath.Join(o.CodexHome, "archived_sessions", filepath.Base(r.path))
			if _, err := publishNativeFile(ctx, r.path, target); err != nil {
				return err
			}
		}
	}
	for _, name := range nativeDBs {
		src := filepath.Join(before, "native", "db", name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		if err := repairDB(ctx, src, filepath.Join(after, "native", "db", name), filepath.Join(o.SQLiteHome, name), true, m.ThreadIDs); err != nil {
			return err
		}
	}
	projectionIDs := append(append([]string{}, ids...), m.ThreadIDs...)
	if err := invalidateHistoryProjection(ctx, o.SQLiteHome, projectionIDs); err != nil {
		return err
	}
	matched, err := existingNative(ctx, o, m, after)
	if err != nil {
		return err
	}
	if !matched {
		return fmt.Errorf("native repair did not produce the verified snapshot")
	}
	return nil
}

func publishManagedAttachments(ctx context.Context, o RestoreOptions, m Manifest, stage string) error {
	for _, e := range m.Files {
		if e.SourcePath == "" {
			continue
		}
		target := filepath.Join(o.CodexHome, "archive_attachments", m.ArchiveID, filepath.Base(filepath.Dir(e.Path)), filepath.Base(e.Path))
		if _, err := publishNativeFile(ctx, filepath.Join(stage, filepath.FromSlash(e.Path)), target); err != nil {
			return err
		}
	}
	return nil
}

func replaceNativeRollout(ctx context.Context, before, after, current *rollout) error {
	if before == nil || after == nil || current == nil {
		return fmt.Errorf("native repair is missing a rollout")
	}
	if rolloutsEqual(ctx, after, current, false) == nil {
		return nil
	}
	if err := rolloutsEqual(ctx, before, current, false); err != nil {
		return err
	}
	if err := checkDestinationParents(current.path); err != nil {
		return err
	}
	info, err := os.Lstat(current.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrConflict
	}
	tmp, err := os.CreateTemp(filepath.Dir(current.path), ".native-repair-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	source, err := os.Open(after.path)
	if err != nil {
		tmp.Close()
		return err
	}
	var writer io.Writer = tmp
	var compressor *zstd.Encoder
	if strings.HasSuffix(current.path, ".zst") {
		compressor, err = zstd.NewWriter(tmp)
		if err != nil {
			source.Close()
			tmp.Close()
			return err
		}
		writer = compressor
	}
	_, copyErr := io.Copy(writer, &contextReader{ctx: ctx, r: source})
	source.Close()
	if compressor != nil {
		err = compressor.Close()
	}
	if copyErr != nil {
		tmp.Close()
		return copyErr
	}
	if err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	latest, err := os.Lstat(current.path)
	if err != nil {
		return err
	}
	if !latest.Mode().IsRegular() || !os.SameFile(info, latest) {
		return fmt.Errorf("%w: native rollout moved during repair", ErrConflict)
	}
	if err := rolloutsEqual(ctx, before, current, false); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), current.path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(current.path))
}

func repairDB(ctx context.Context, before, after, target string, apply bool, ids []string) error {
	old, err := openDB(before, true)
	if err != nil {
		return err
	}
	defer old.Close()
	desired, err := openDB(after, true)
	if err != nil {
		return err
	}
	defer desired.Close()
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrConflict
	}
	if err := checkDestinationParents(target); err != nil {
		return err
	}
	current, err := openDB(target, !apply)
	if err != nil {
		return err
	}
	defer current.Close()
	tx, err := current.BeginTx(ctx, &sql.TxOptions{ReadOnly: !apply})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := old.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='_sqlx_migrations'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, table)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, table := range tables {
		cols, err := columnNames(ctx, old, table)
		if err != nil {
			return err
		}
		scoped := table == "threads" || table == "thread_spawn_edges"
		for _, col := range cols {
			scoped = scoped || col == "thread_id"
		}
		if !scoped {
			continue
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		queryArgs := make([]any, len(ids))
		for i, id := range ids {
			queryArgs[i] = id
		}
		where := "thread_id IN (" + marks + ")"
		if table == "threads" {
			where = "id IN (" + marks + ")"
		}
		if table == "thread_spawn_edges" {
			where = "parent_thread_id IN (" + marks + ") OR child_thread_id IN (" + marks + ")"
			queryArgs = append(append([]any{}, queryArgs...), queryArgs...)
		}
		var expectedCount, currentCount int
		if err := old.QueryRowContext(ctx, "SELECT count(*) FROM "+quote(table)).Scan(&expectedCount); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+quote(table)+" WHERE "+where, queryArgs...).Scan(&currentCount); err != nil {
			return err
		}
		if expectedCount != currentCount {
			return fmt.Errorf("%w: native repair row set changed in %s", ErrConflict, table)
		}
		keys, err := primaryKeys(ctx, old, table)
		if err != nil {
			return err
		}
		rows, err := old.QueryContext(ctx, "SELECT rowid,* FROM "+quote(table))
		if err != nil {
			return err
		}
		for rows.Next() {
			beforeValues := make([]any, len(cols)+1)
			pointers := make([]any, len(beforeValues))
			for i := range pointers {
				pointers[i] = &beforeValues[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				return err
			}
			afterValues, err := scanRow(desired.QueryRowContext(ctx, "SELECT * FROM "+quote(table)+" WHERE rowid=?", beforeValues[0]), len(cols))
			if err != nil {
				rows.Close()
				return err
			}
			beforeValues = beforeValues[1:]
			var terms []string
			var keyValues []any
			for _, key := range keys {
				for i, col := range cols {
					if col == key {
						terms = append(terms, quote(key)+" IS ?")
						keyValues = append(keyValues, beforeValues[i])
					}
				}
			}
			if len(terms) == 0 {
				rows.Close()
				return fmt.Errorf("unsupported native repair row key")
			}
			actual, err := scanRow(tx.QueryRowContext(ctx, "SELECT * FROM "+quote(table)+" WHERE "+strings.Join(terms, " AND "), keyValues...), len(cols))
			if err != nil {
				rows.Close()
				return fmt.Errorf("%w: native repair row is missing in %s", ErrConflict, table)
			}
			var changes []string
			var values []any
			for i, col := range cols {
				if table == "threads" && lifecycleColumn(col) {
					continue
				}
				if !reflect.DeepEqual(actual[i], beforeValues[i]) && !reflect.DeepEqual(actual[i], afterValues[i]) {
					rows.Close()
					return fmt.Errorf("%w: native repair preimage differs in %s.%s", ErrConflict, table, col)
				}
				if !reflect.DeepEqual(beforeValues[i], afterValues[i]) {
					changes = append(changes, quote(col)+"=?")
					values = append(values, afterValues[i])
				}
			}
			if apply && len(changes) > 0 {
				values = append(values, keyValues...)
				if _, err := tx.ExecContext(ctx, "UPDATE "+quote(table)+" SET "+strings.Join(changes, ",")+" WHERE "+strings.Join(terms, " AND "), values...); err != nil {
					rows.Close()
					return err
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func lifecycleColumn(column string) bool {
	return column == "archived" || column == "archived_at" || column == "rollout_path" || column == "updated_at" || column == "updated_at_ms"
}
func scanRow(row *sql.Row, n int) ([]any, error) {
	values := make([]any, n)
	pointers := make([]any, n)
	for i := range pointers {
		pointers[i] = &values[i]
	}
	err := row.Scan(pointers...)
	return values, err
}
func primaryKeys(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quote(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type primary struct {
		order int
		name  string
	}
	var keys []primary
	for rows.Next() {
		var cid, notnull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notnull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		if pk > 0 {
			keys = append(keys, primary{pk, name})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].order < keys[j].order })
	var names []string
	for _, key := range keys {
		names = append(names, key.name)
	}
	return names, nil
}

func invalidateHistoryProjection(ctx context.Context, home string, ids []string) error {
	filename := filepath.Join(home, "thread_history_1.sqlite")
	if info, err := os.Lstat(filename); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: native projection database is not regular", ErrConflict)
	}
	if err := checkDestinationParents(filename); err != nil {
		return err
	}
	db, err := openDB(filename, false)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='_sqlx_migrations'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range tables {
		cols, err := columnNames(ctx, tx, table)
		if err != nil {
			return err
		}
		for _, col := range cols {
			if col == "thread_id" {
				for _, id := range ids {
					if _, err := tx.ExecContext(ctx, "DELETE FROM "+quote(table)+" WHERE thread_id=?", id); err != nil {
						return err
					}
				}
				break
			}
		}
	}
	return tx.Commit()
}
