package sessionarchive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Restore validates the entire archive before publishing the project. Existing
// native records are accepted only when their canonical history still matches
// the snapshot, so a newer thread can never be silently replaced by an old archive.
func Restore(ctx context.Context, o RestoreOptions) (Manifest, error) {
	var m Manifest
	if err := supportedVersion(o.CodexVersion); err != nil {
		return m, err
	}
	if o.SQLiteHome == "" {
		o.SQLiteHome = o.CodexHome
	}
	if o.CodexHome == "" {
		return m, fmt.Errorf("missing Codex home")
	}
	if err := os.MkdirAll(o.CodexHome, 0700); err != nil {
		return m, err
	}
	home, err := checkedRoot(o.CodexHome)
	if err != nil {
		return m, err
	}
	o.CodexHome = home
	if err := os.MkdirAll(o.SQLiteHome, 0700); err != nil {
		return m, err
	}
	o.SQLiteHome, err = checkedRoot(o.SQLiteHome)
	if err != nil {
		return m, err
	}
	stage, err := os.MkdirTemp(filepath.Dir(home), ".archive-restore-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(stage)
	progress(o.Progress, "Checking archive checksums")
	m, err = unpack(ctx, o.ArchivePath, stage)
	if err != nil {
		return Manifest{}, err
	}
	if err := CheckVersionCompatibility(m.CodexVersion, o.CodexVersion); err != nil {
		return Manifest{}, err
	}
	if err := validateNativeSnapshot(ctx, stage, m); err != nil {
		return Manifest{}, err
	}
	if err := validateNativeRestoreTarget(ctx, o, m); err != nil {
		return Manifest{}, err
	}
	if o.Destination == "" {
		o.Destination = m.OriginalPath
	}
	o.Destination, err = filepath.Abs(o.Destination)
	if err != nil {
		return Manifest{}, err
	}
	projectExists := false
	if _, err := os.Lstat(o.Destination); err == nil {
		projectExists = true
	} else if !os.IsNotExist(err) {
		return Manifest{}, err
	}
	if within(o.Destination, home) || within(home, o.Destination) {
		return Manifest{}, fmt.Errorf("project and Codex home must be separate")
	}
	if err := checkDestinationParents(o.Destination); err != nil {
		return Manifest{}, err
	}
	if projectExists {
		if err := existingProjectMatches(ctx, o.Destination, m); err != nil {
			return Manifest{}, err
		}
	}
	interrupted, repairMode, err := interruptedImportMode(ctx, o, m)
	if err != nil {
		return Manifest{}, err
	}
	o.repairNative = repairMode
	existing, err := existingNative(ctx, o, m, stage)
	retained := existing && o.CodexHome == m.OriginalCodexHome && o.Destination == m.OriginalPath && !interrupted
	var beforeStage string
	if retained {
		for _, entry := range m.Files {
			if entry.SourcePath != "" {
				o.repairNative = true
				retained = false
				break
			}
		}
	}
	if o.repairNative {
		if o.CodexHome != m.OriginalCodexHome || o.Destination != m.OriginalPath {
			return Manifest{}, fmt.Errorf("%w: retained native replacement must use the original project and home", ErrConflict)
		}
		beforeStage = filepath.Join(stage, "preimage")
		if err := copyTree(ctx, filepath.Join(stage, "native"), filepath.Join(beforeStage, "native")); err != nil {
			return Manifest{}, err
		}
		o.rolloutAliases, err = repairAliases(ctx, m, beforeStage)
		if err != nil {
			return Manifest{}, err
		}
		if err := rebindNative(ctx, o, m, stage); err != nil {
			return Manifest{}, err
		}
		existing, err = existingNative(ctx, o, m, stage)
		if existing {
			if err := publishManagedAttachments(ctx, o, m, stage); err != nil {
				return Manifest{}, err
			}
		}
	} else if !retained {
		if err := rebindNative(ctx, o, m, stage); err != nil {
			return Manifest{}, err
		}
		existing, err = existingNative(ctx, o, m, stage)
	}
	reboundManifest := m
	reboundManifest.OriginalCodexHome = o.CodexHome
	reboundManifest.OriginalPath = o.Destination
	if !retained {
		if err := validateNativeSnapshot(ctx, stage, reboundManifest); err != nil {
			return Manifest{}, err
		}
	}
	if err != nil && !interrupted && !o.repairNative {
		return Manifest{}, err
	}
	if interrupted && !o.repairNative {
		existing = false
	}
	if !existing {
		progress(o.Progress, "Restoring native Codex history and metadata")
	}
	// Stage the project on the destination filesystem so final publication is atomic.
	if err := os.MkdirAll(filepath.Dir(o.Destination), 0700); err != nil {
		return Manifest{}, err
	}
	projectStage, err := os.MkdirTemp(filepath.Dir(o.Destination), ".project-restore-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(projectStage)
	if !projectExists {
		if err := copyTree(ctx, filepath.Join(stage, "project"), filepath.Join(projectStage, "project")); err != nil {
			return Manifest{}, err
		}
	}
	var undo func()
	if !existing {
		if err := beginImportIntent(ctx, o, m); err != nil {
			return Manifest{}, err
		}
		if o.repairNative {
			err = repairRetainedNative(ctx, o, m, beforeStage, stage)
			undo = func() {}
		} else {
			undo, err = importNative(ctx, o, m, stage)
		}
		if err != nil {
			return Manifest{}, err
		}
		matched, err := existingNative(ctx, o, m, stage)
		if err != nil {
			return Manifest{}, err
		}
		if !matched {
			return Manifest{}, fmt.Errorf("native import did not produce the verified snapshot")
		}
	}
	// Linux renameat2 prevents replacing a destination created during restore.
	if !projectExists {
		if err := publishDirectory(filepath.Join(projectStage, "project"), o.Destination); err != nil {
			if undo != nil {
				undo()
			}
			return Manifest{}, err
		}
	}
	progress(o.Progress, "Project and native session restored")
	// Keep the tiny repair recipe to recognize deterministic frozen ancestor
	// IDs after a later worker restart. A matching postimage is a safe no-op.
	if !o.repairNative {
		if err := os.Remove(intentPath(o, m)); err != nil && !os.IsNotExist(err) {
			return Manifest{}, err
		}
	}
	return m, nil
}

func existingProjectMatches(ctx context.Context, directory string, m Manifest) error {
	return CheckProject(ctx, directory, m)
}

func checkDestinationParents(destination string) error {
	for p := filepath.Dir(destination); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("unsafe restore parent %s", p)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}

func existingNative(ctx context.Context, o RestoreOptions, m Manifest, stage string) (bool, error) {
	filename := filepath.Join(o.SQLiteHome, "state_5.sqlite")
	present := false
	if _, err := os.Stat(filename); err == nil {
		db, err := openDB(filename, true)
		if err != nil {
			return false, err
		}
		defer db.Close()
		if tableExists(ctx, db, "threads") {
			for _, id := range m.ThreadIDs {
				var n int
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM threads WHERE id=?", id).Scan(&n); err != nil {
					return false, err
				}
				present = present || n != 0
			}
		}
	} else if !os.IsNotExist(err) {
		return false, err
	}
	all, err := scanRollouts(ctx, o.CodexHome)
	if err != nil {
		return false, err
	}
	owners := map[string]bool{}
	for _, id := range m.ThreadIDs {
		owners[id] = true
	}
	for _, r := range all {
		present = present || owners[r.owner]
	}
	if !present {
		return false, nil
	}
	expected, err := scanRollouts(ctx, filepath.Join(stage, "native", "home"))
	if err != nil {
		return false, err
	}
	for id, want := range expected {
		got := all[id]
		if got == nil {
			return false, fmt.Errorf("%w: native history is incomplete", ErrConflict)
		}
		if err := rolloutsEqual(ctx, want, got, !owners[want.owner]); err != nil {
			return false, err
		}
	}
	for id, r := range all {
		if owners[r.owner] && expected[id] == nil {
			return false, fmt.Errorf("%w: native thread has newer rollout generations", ErrConflict)
		}
	}
	// Archive/unarchive changes only these lifecycle fields; every other
	// per-thread metadata value must still agree with the snapshot.
	for _, name := range nativeDBs {
		source := filepath.Join(stage, "native", "db", name)
		if _, err := os.Stat(source); os.IsNotExist(err) {
			continue
		}
		target := filepath.Join(o.SQLiteHome, name)
		if err := compareNativeRows(ctx, source, target, m.ThreadIDs); err != nil {
			return false, err
		}
	}
	return true, nil
}

func compareNativeRows(ctx context.Context, source, target string, ids []string) error {
	src, err := openDB(source, true)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := openDB(target, true)
	if err != nil {
		return fmt.Errorf("%w: native metadata missing", ErrConflict)
	}
	defer dst.Close()
	for _, table := range []string{"threads", "thread_dynamic_tools", "thread_spawn_edges", "thread_attachments", "thread_goals", "thread_goal_continuation_deferrals", "thread_read_receipts"} {
		if !tableExists(ctx, src, table) {
			continue
		}
		rows, err := src.QueryContext(ctx, "SELECT * FROM "+quote(table))
		if err != nil {
			return err
		}
		cols, _ := rows.Columns()
		expectedCount := 0
		for rows.Next() {
			expectedCount++
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				return err
			}
			var terms []string
			var args []any
			for i, c := range cols {
				if table == "threads" && (c == "archived" || c == "archived_at" || c == "rollout_path" || c == "updated_at" || c == "updated_at_ms") {
					continue
				}
				terms = append(terms, quote(c)+" IS ?")
				args = append(args, values[i])
			}
			var n int
			if err := dst.QueryRowContext(ctx, "SELECT count(*) FROM "+quote(table)+" WHERE "+strings.Join(terms, " AND "), args...).Scan(&n); err != nil || n != 1 {
				rows.Close()
				if table == "threads" {
					var differences []string
					for i, col := range cols {
						if col == "archived" || col == "archived_at" || col == "rollout_path" || col == "updated_at" || col == "updated_at_ms" {
							continue
						}
						var identical int
						if dst.QueryRowContext(ctx, "SELECT count(*) FROM threads WHERE id=? AND "+quote(col)+" IS ?", values[0], values[i]).Scan(&identical) == nil && identical != 1 {
							differences = append(differences, col)
						}
					}
					return fmt.Errorf("%w: native metadata differs in threads (%s)", ErrConflict, strings.Join(differences, ", "))
				}
				return fmt.Errorf("%w: native metadata differs in %s", ErrConflict, table)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		where := "thread_id IN (" + marks + ")"
		if table == "threads" {
			where = "id IN (" + marks + ")"
		}
		if table == "thread_spawn_edges" {
			where = "parent_thread_id IN (" + marks + ") OR child_thread_id IN (" + marks + ")"
			args = append(append([]any{}, args...), args...)
		}
		var actualCount int
		if err := dst.QueryRowContext(ctx, "SELECT count(*) FROM "+quote(table)+" WHERE "+where, args...).Scan(&actualCount); err != nil || actualCount != expectedCount {
			return fmt.Errorf("%w: native metadata has newer or missing rows in %s", ErrConflict, table)
		}
	}
	return nil
}

func rebindNative(ctx context.Context, o RestoreOptions, m Manifest, stage string) error {
	replacements := map[string]string{m.OriginalCodexHome: o.CodexHome, m.OriginalPath: o.Destination}
	for _, e := range m.Files {
		if e.SourcePath != "" {
			replacements[e.SourcePath] = filepath.Join(o.CodexHome, "archive_attachments", m.ArchiveID, filepath.Base(filepath.Dir(e.Path)), filepath.Base(e.Path))
		}
	}
	all, err := scanRollouts(ctx, filepath.Join(stage, "native", "home"))
	if err != nil {
		return err
	}
	offsets := map[string]map[int64]int64{}
	for id := range all {
		offsets[id] = map[int64]int64{0: 0}
	}
	for _, r := range all {
		if err := readRolloutMetadata(ctx, r); err != nil {
			return err
		}
		if r.base != "" {
			if all[r.base] == nil {
				return fmt.Errorf("native lineage source is missing")
			}
			offsets[r.base][r.end] = r.end
		}
	}
	// Only fork cutoff offsets are retained in memory. Rollout contents are
	// streamed to private temporary files, including across large model histories.
	outputs := map[string]string{}
	for iteration := 0; iteration < len(all)+8; iteration++ {
		next := map[string]map[int64]int64{}
		for id, r := range all {
			if prior := outputs[id]; prior != "" {
				os.Remove(prior)
			}
			output := r.path + ".rewrite"
			outputs[id] = output
			mapping, err := rewriteRolloutFile(ctx, r.path, output, replacements, offsets, offsets[id], o.rolloutAliases)
			if err != nil {
				return err
			}
			next[id] = mapping
		}
		if reflect.DeepEqual(next, offsets) {
			break
		}
		if iteration == len(all)+7 {
			return fmt.Errorf("native lineage offsets did not stabilize")
		}
		offsets = next
	}
	for id, filename := range outputs {
		target := all[id].path
		if alias := o.rolloutAliases[id]; alias != "" {
			base := filepath.Base(target)
			target = filepath.Join(filepath.Dir(target), base[:28]+all[id].owner+"_"+alias+".jsonl")
		}
		if err := os.Rename(filename, target); err != nil {
			return err
		}
		if target != all[id].path {
			if err := os.Remove(all[id].path); err != nil {
				return err
			}
		}
	}
	for _, name := range nativeDBs {
		filename := filepath.Join(stage, "native", "db", name)
		if _, err := os.Stat(filename); os.IsNotExist(err) {
			continue
		}
		if err := rewriteDB(ctx, filename, replacements); err != nil {
			return err
		}
	}
	return nil
}

func scanNativeLine(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) != 0 {
		return 0, nil, fmt.Errorf("native rollout ends inside a record")
	}
	return 0, nil, nil
}

func rewriteRolloutFile(ctx context.Context, source, target string, replacements map[string]string, offsets map[string]map[int64]int64, needed map[int64]int64, aliases map[string]string) (map[int64]int64, error) {
	mapping := map[int64]int64{0: 0}
	in, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	writer := bufio.NewWriter(out)
	scanner := bufio.NewScanner(&contextReader{ctx: ctx, r: in})
	scanner.Buffer(make([]byte, 4096), maxNativeLineBytes)
	scanner.Split(scanNativeLine)
	var oldOffset, newOffset int64
	for scanner.Scan() {
		line := scanner.Bytes()
		value, err := decodeNativeJSON(line)
		if err != nil {
			return nil, err
		}
		before, _ := json.Marshal(value)
		rewriteValue(value, replacements, offsets, aliases)
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		written := line
		if !bytes.Equal(before, encoded) {
			written = encoded
		}
		if _, err := writer.Write(written); err != nil {
			return nil, err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return nil, err
		}
		oldOffset += int64(len(line) + 1)
		newOffset += int64(len(written) + 1)
		if _, ok := needed[oldOffset]; ok {
			mapping[oldOffset] = newOffset
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for old := range needed {
		if _, ok := mapping[old]; !ok {
			return nil, fmt.Errorf("native lineage cutoff does not end on a record")
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	return mapping, out.Close()
}

func rolloutsEqual(ctx context.Context, want, got *rollout, prefix bool) error {
	a, err := openRollout(want.path)
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := openRollout(got.path)
	if err != nil {
		return err
	}
	defer b.Close()
	info, err := os.Stat(want.path)
	if err != nil {
		return err
	}
	ha, hb := sha256.New(), sha256.New()
	if _, err := io.Copy(ha, &contextReader{ctx: ctx, r: a}); err != nil {
		return err
	}
	var n int64
	if prefix {
		n, err = io.CopyN(hb, &contextReader{ctx: ctx, r: b}, info.Size())
	} else {
		n, err = io.Copy(hb, &contextReader{ctx: ctx, r: b})
	}
	if err != nil || n != info.Size() || !bytes.Equal(ha.Sum(nil), hb.Sum(nil)) {
		return fmt.Errorf("%w: native history changed since this archive", ErrConflict)
	}
	return nil
}

func decodeNativeJSON(b []byte) (any, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	err := decoder.Decode(&value)
	return value, err
}
func rebound(s string, replacements map[string]string) string {
	var keys []string
	for old := range replacements {
		keys = append(keys, old)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, old := range keys {
		if s == old {
			return replacements[old]
		}
		if strings.HasPrefix(s, old+string(filepath.Separator)) {
			return replacements[old] + strings.TrimPrefix(s, old)
		}
	}
	return s
}
func rewriteValue(value any, replacements map[string]string, offsets map[string]map[int64]int64, aliases ...map[string]string) {
	switch v := value.(type) {
	case map[string]any:
		for k, x := range v {
			if s, ok := x.(string); ok {
				v[k] = rebound(s, replacements)
			} else {
				rewriteValue(x, replacements, offsets, aliases...)
			}
		}
		if raw, ok := v["history_base"].(map[string]any); ok {
			if id, ok := raw["thread_id"].(string); ok {
				if n, ok := raw["end_byte_offset"].(json.Number); ok {
					old, _ := n.Int64()
					if updated, ok := offsets[id][old]; ok {
						raw["end_byte_offset"] = updated
					}
				}
			}
		}
		if raw, ok := v["history_base"].(map[string]any); ok && len(aliases) > 0 {
			if id, ok := raw["thread_id"].(string); ok {
				if alias := aliases[0][id]; alias != "" {
					raw["thread_id"] = alias
				}
			}
		}

	case []any:
		for i, x := range v {
			if s, ok := x.(string); ok {
				v[i] = rebound(s, replacements)
			} else {
				rewriteValue(x, replacements, offsets, aliases...)
			}
		}
	}
}

func rewriteDB(ctx context.Context, filename string, replacements map[string]string) error {
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
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, "SELECT rowid,* FROM "+quote(table))
		if err != nil {
			return err
		}
		cols, _ := rows.Columns()
		type update struct {
			rowid  any
			values []any
		}
		var updates []update
		for rows.Next() {
			v := make([]any, len(cols))
			ptr := make([]any, len(v))
			for i := range v {
				ptr[i] = &v[i]
			}
			if err := rows.Scan(ptr...); err != nil {
				rows.Close()
				return err
			}
			changed := false
			for i := 1; i < len(v); i++ {
				if s, ok := v[i].(string); ok {
					replacement := rebound(s, replacements)
					if json.Valid([]byte(s)) {
						value, err := decodeNativeJSON([]byte(s))
						if err == nil {
							before, _ := json.Marshal(value)
							rewriteValue(value, replacements, nil)
							encoded, _ := json.Marshal(value)
							if !bytes.Equal(before, encoded) {
								replacement = string(encoded)
							}
						}
					}
					if replacement != s {
						changed = true
						v[i] = replacement
					}
				}
			}
			if changed {
				updates = append(updates, update{v[0], v[1:]})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, u := range updates {
			var terms []string
			for _, name := range cols[1:] {
				terms = append(terms, quote(name)+"=?")
			}
			args := append(u.values, u.rowid)
			if _, err := db.ExecContext(ctx, "UPDATE "+quote(table)+" SET "+strings.Join(terms, ",")+" WHERE rowid=?", args...); err != nil {
				return err
			}
		}
	}
	return nil
}

func importNative(ctx context.Context, o RestoreOptions, m Manifest, stage string) (func(), error) {
	// Native files/rows published before a process interruption belong to the
	// durable import intent. Preserve them for verified reconciliation on retry;
	// never delete a native record that another process may have since resumed.
	undo := func() {}
	fail := func(err error) (func(), error) { return nil, err }
	err := filepath.WalkDir(filepath.Join(stage, "native", "home"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(stage, "native", "home"), p)
		if rel == "session_index.jsonl" {
			return nil
		}
		target := filepath.Join(o.CodexHome, rel)
		if err := checkDestinationParents(target); err != nil {
			return err
		}
		_, err = publishNativeFile(ctx, p, target)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	for _, e := range m.Files {
		if e.SourcePath == "" {
			continue
		}
		src := filepath.Join(stage, filepath.FromSlash(e.Path))
		target := filepath.Join(o.CodexHome, "archive_attachments", m.ArchiveID, filepath.Base(filepath.Dir(e.Path)), filepath.Base(e.Path))
		_, err := publishNativeFile(ctx, src, target)
		if err != nil {
			return fail(err)
		}
	}
	for _, name := range nativeDBs {
		src := filepath.Join(stage, "native", "db", name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		target := filepath.Join(o.SQLiteHome, name)
		if err := checkDestinationParents(target); err != nil {
			return fail(err)
		}
		if info, err := os.Lstat(target); os.IsNotExist(err) {
			_, err := publishNativeFile(ctx, src, target)
			if err != nil {
				return fail(err)
			}
		} else if err != nil {
			return fail(err)
		} else {
			if !info.Mode().IsRegular() {
				return fail(fmt.Errorf("%w: native database target is not regular", ErrConflict))
			}
			if err := mergeDB(ctx, src, target); err != nil {
				return fail(err)
			}
		}
	}
	return undo, nil
}

func mergeDB(ctx context.Context, source, target string) error {
	src, err := openDB(source, true)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := openDB(target, false)
	if err != nil {
		return err
	}
	defer dst.Close()
	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := src.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='_sqlx_migrations' ORDER BY CASE name WHEN 'thread_sections' THEN 0 WHEN 'projects' THEN 1 WHEN 'threads' THEN 2 ELSE 3 END")
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
	for _, table := range tables {
		rows, err := src.QueryContext(ctx, "SELECT * FROM "+quote(table))
		if err != nil {
			return err
		}
		cols, _ := rows.Columns()
		targetCols, err := columnNames(ctx, tx, table)
		if err != nil {
			rows.Close()
			return fmt.Errorf("%w: target schema lacks %s", ErrUnsupported, table)
		}
		if !reflect.DeepEqual(cols, targetCols) {
			rows.Close()
			return fmt.Errorf("%w: target schema differs in %s", ErrUnsupported, table)
		}
		var names []string
		for _, col := range cols {
			names = append(names, quote(col))
		}
		for rows.Next() {
			v := make([]any, len(cols))
			ptr := make([]any, len(v))
			for i := range v {
				ptr[i] = &v[i]
			}
			if err := rows.Scan(ptr...); err != nil {
				rows.Close()
				return err
			}
			verb := "INSERT OR IGNORE"
			if _, err := tx.ExecContext(ctx, verb+" INTO "+quote(table)+" ("+strings.Join(names, ",")+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+")", v...); err != nil {
				rows.Close()
				return fmt.Errorf("%w: native metadata insert %s: %v", ErrConflict, table, err)
			}
			var terms []string
			for _, column := range cols {
				terms = append(terms, quote(column)+" IS ?")
			}
			var identical int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+quote(table)+" WHERE "+strings.Join(terms, " AND "), v...).Scan(&identical); err != nil || identical != 1 {
				rows.Close()
				return fmt.Errorf("%w: native metadata differs from interrupted import in %s", ErrConflict, table)
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

// ExistingNativeMatches checks the snapshot against a native tree without writing
// either. It is useful before a worker unarchives a retained native session.
func ExistingNativeMatches(ctx context.Context, o RestoreOptions) (bool, error) {
	stage, err := os.MkdirTemp("", "archive-match-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(stage)
	m, err := unpack(ctx, o.ArchivePath, stage)
	if err != nil {
		return false, err
	}
	if o.SQLiteHome == "" {
		o.SQLiteHome = o.CodexHome
	}
	return existingNative(ctx, o, m, stage)
}

// Digest is exposed for operation journals and inventory consistency checks.
func Digest(ctx context.Context, filename string) (string, error) {
	f, err := os.OpenFile(filename, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return "", err
	} else if !info.Mode().IsRegular() {
		return "", fmt.Errorf("checksum source is not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, &contextReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
