package sessionarchive

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

var nativeDBs = []string{"state_5.sqlite", "goals_1.sqlite", "queue_1.sqlite", "memories_1.sqlite"}

type rollout struct {
	path, relative, owner, id string
	base                      string
	end                       int64
	metadataRead              bool
}

func nativeVersion(version string) string {
	return strings.TrimSpace(strings.TrimPrefix(version, "codex-cli "))
}

func supportedVersion(version string) error {
	version = nativeVersion(version)
	if _, ok := nativeSchemaFingerprints[version]; !ok {
		return fmt.Errorf("%w: Codex version %q has not been validated", ErrUnsupported, version)
	}
	return nil
}

// CheckCompatibility lets a worker reject an unvalidated native layout before
// asking that runtime to perform archive-specific inventory or lifecycle RPCs.
func CheckCompatibility(version string) error { return supportedVersion(version) }

// CheckVersionCompatibility rejects cross-version imports until native schema
// migration and replay have been proven for that exact source/target pair.
func CheckVersionCompatibility(archiveVersion, runtimeVersion string) error {
	if err := supportedVersion(archiveVersion); err != nil {
		return err
	}
	if err := supportedVersion(runtimeVersion); err != nil {
		return err
	}
	if nativeVersion(archiveVersion) != nativeVersion(runtimeVersion) {
		return fmt.Errorf("%w: archive created with Codex %s requires the same runtime version; selected runtime is %s", ErrUnsupported, nativeVersion(archiveVersion), nativeVersion(runtimeVersion))
	}
	return nil
}

func openDB(filename string, readonly bool) (*sql.DB, error) {
	if info, err := os.Lstat(filename); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("native database is not a regular file")
		}
	} else if !os.IsNotExist(err) || readonly {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filename}
	q := u.Query()
	if readonly {
		q.Set("mode", "ro")
	}
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func tableExists(ctx context.Context, db *sql.DB, name string) bool {
	var n int
	return db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n) == nil && n == 1
}
func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func threadTree(ctx context.Context, sqliteHome, root string) ([]string, error) {
	ids := []string{root}
	filename := filepath.Join(sqliteHome, "state_5.sqlite")
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return ids, nil
	}
	db, err := openDB(filename, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if !tableExists(ctx, db, "thread_spawn_edges") {
		return ids, nil
	}
	seen := map[string]bool{root: true}
	for i := 0; i < len(ids); i++ {
		rows, err := db.QueryContext(ctx, "SELECT child_thread_id FROM thread_spawn_edges WHERE parent_thread_id=?", ids[i])
		if err != nil {
			return nil, err
		}
		var children []string
		for rows.Next() {
			var child string
			if err := rows.Scan(&child); err != nil {
				rows.Close()
				return nil, err
			}
			if _, err := uuid.Parse(child); err != nil {
				rows.Close()
				return nil, fmt.Errorf("invalid descendant ID")
			}
			children = append(children, child)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		sort.Strings(children)
		for _, child := range children {
			if !seen[child] {
				seen[child] = true
				ids = append(ids, child)
			}
		}
	}
	// Attached side threads introduced in 0.165 have a separate lifecycle from
	// spawned subagents. Until that lifecycle is independently proven, refuse
	// both ends of an attached relationship rather than export an incomplete tree.
	if err := checkAttachedThreads(ctx, db, ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func checkAttachedThreads(ctx context.Context, db *sql.DB, ids []string) error {
	columns, err := columnNames(ctx, db, "threads")
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column != "attached_thread_parent_id" {
			continue
		}
		for _, id := range ids {
			var attached int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM threads WHERE (id=? AND attached_thread_parent_id IS NOT NULL) OR attached_thread_parent_id=?", id, id).Scan(&attached); err != nil {
				return err
			}
			if attached != 0 {
				return fmt.Errorf("%w: attached side threads have not been validated for archival", ErrUnsupported)
			}
		}
		break
	}
	return nil
}

const maxNativeLineBytes = 32 << 20

// enumerateRollout uses the native canonical basename only. Unrelated session
// contents, including compressed contents, are never opened during discovery.
func enumerateRollout(filename, root string) (rollout, error) {
	var r rollout
	r.path = filename
	r.relative, _ = filepath.Rel(root, filename)
	stem := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(filename), ".zst"), ".jsonl")
	if !strings.HasPrefix(stem, "rollout-") || len(stem) < len("rollout-")+20+36 {
		return r, fmt.Errorf("invalid rollout filename")
	}
	parts := strings.Split(strings.TrimPrefix(stem, "rollout-")[20:], "_")
	if strings.TrimPrefix(stem, "rollout-")[19] != '-' {
		return r, fmt.Errorf("invalid rollout filename delimiter")
	}
	if len(parts) > 2 {
		return r, fmt.Errorf("invalid rollout filename")
	}
	r.owner = parts[0]
	r.id = parts[len(parts)-1]
	if _, err := uuid.Parse(r.owner); err != nil {
		return r, err
	}
	if _, err := uuid.Parse(r.id); err != nil {
		return r, err
	}
	r.relative = strings.TrimSuffix(r.relative, ".zst")
	return r, nil
}

type nativeReader struct {
	io.Reader
	close func()
}

func (r *nativeReader) Close() error { r.close(); return nil }
func openRollout(filename string) (io.ReadCloser, error) {
	f, err := os.OpenFile(filename, os.O_RDONLY|noFollowFlag(), 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("native rollout is not a regular file")
	}
	if strings.HasSuffix(filename, ".zst") {
		z, err := zstd.NewReader(f, zstd.WithDecoderMaxMemory(256<<20))
		if err != nil {
			f.Close()
			return nil, err
		}
		return &nativeReader{Reader: z, close: func() { z.Close(); f.Close() }}, nil
	}
	return f, nil
}
func readRolloutMetadata(ctx context.Context, r *rollout) error {
	if r.metadataRead {
		return nil
	}
	f, err := openRollout(r.path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(&contextReader{ctx: ctx, r: f})
	scanner.Buffer(make([]byte, 4096), maxNativeLineBytes)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return err
		}
		return fmt.Errorf("native rollout has no metadata")
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID          string `json:"id"`
			HistoryBase *struct {
				ThreadID string `json:"thread_id"`
				End      int64  `json:"end_byte_offset"`
			} `json:"history_base"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &meta); err != nil || meta.Type != "session_meta" || meta.Payload.ID != r.owner {
		return fmt.Errorf("invalid native rollout metadata: %s", r.path)
	}
	if meta.Payload.HistoryBase != nil {
		r.base = meta.Payload.HistoryBase.ThreadID
		r.end = meta.Payload.HistoryBase.End
		if r.end <= 0 {
			return fmt.Errorf("invalid native lineage cutoff")
		}
		if _, err := uuid.Parse(r.base); err != nil {
			return err
		}
	}
	r.metadataRead = true
	return nil
}

func spoolRollout(ctx context.Context, r *rollout, target string, limit int64) error {
	before, err := os.Lstat(r.path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("native rollout is not a regular file")
	}
	in, err := openRollout(r.path)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	reader := &contextReader{ctx: ctx, r: in}
	var n int64
	if limit < 0 {
		n, err = io.Copy(out, reader)
	} else {
		n, err = io.CopyN(out, reader, limit)
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("native rollout is empty")
	}
	var tail [1]byte
	if _, err := out.ReadAt(tail[:], n-1); err != nil {
		return err
	}
	if tail[0] != '\n' {
		return fmt.Errorf("native rollout ends inside a record")
	}
	after, err := os.Lstat(r.path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("native rollout changed during archive")
	}
	return out.Close()
}

func scanRollouts(ctx context.Context, home string) (map[string]*rollout, error) {
	result := map[string]*rollout{}
	for _, sub := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(home, sub)
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(p, ".jsonl") && !strings.HasSuffix(p, ".jsonl.zst") {
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink in native rollout store")
			}
			r, err := enumerateRollout(p, home)
			if err != nil {
				return nil
			}
			if old, ok := result[r.id]; !ok || strings.HasSuffix(old.path, ".zst") {
				result[r.id] = &r
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func exportNative(ctx context.Context, o ExportOptions, stage string, m *Manifest) error {
	if err := supportedVersion(o.CodexVersion); err != nil {
		return err
	}
	ids, err := threadTree(ctx, o.SQLiteHome, o.ThreadID)
	if err != nil {
		return err
	}
	m.ThreadIDs = ids
	selected := map[string]bool{}
	for _, id := range ids {
		selected[id] = true
	}
	all, err := scanRollouts(ctx, o.CodexHome)
	if err != nil {
		return err
	}
	include := map[string]int64{}
	owned := map[string]bool{}
	for id, r := range all {
		if selected[r.owner] {
			include[id] = -1
			owned[id] = true
		}
	}
	if len(include) == 0 {
		return fmt.Errorf("no durable native rollout exists for thread %s", o.ThreadID)
	}
	// Freeze ancestor segments at the greatest inherited prefix needed by this
	// tree. Unrelated continuations of a fork source never enter the archive.
	for changed := true; changed; {
		changed = false
		for id := range include {
			r := all[id]
			if err := readRolloutMetadata(ctx, r); err != nil {
				return err
			}
			if r.base == "" || r.base == id {
				continue
			}
			base := all[r.base]
			if base == nil {
				return fmt.Errorf("native history source %s is unavailable", r.base)
			}
			if old, ok := include[r.base]; !ok || (old >= 0 && old < r.end) {
				include[r.base] = r.end
				changed = true
			}
		}
	}
	for id, limit := range include {
		r := all[id]
		rel := r.relative
		if !owned[id] {
			rel = filepath.Join("archived_sessions", filepath.Base(rel))
		}
		target := filepath.Join(stage, "native", "home", rel)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := spoolRollout(ctx, r, target, limit); err != nil {
			return err
		}
		f, err := os.Open(target)
		if err != nil {
			return err
		}
		err = collectAttachmentReader(ctx, f, o, stage, m)
		f.Close()
		if err != nil {
			return err
		}
	}
	for _, name := range nativeDBs {
		source := filepath.Join(o.SQLiteHome, name)
		if _, err := os.Stat(source); os.IsNotExist(err) {
			continue
		}
		target := filepath.Join(stage, "native", "db", name)
		if err := snapshotDB(ctx, source, target, ids, o, stage, m); err != nil {
			return fmt.Errorf("snapshot %s: %w", name, err)
		}
	}
	// Keep the compatibility name index limited to this tree.
	if f, err := os.Open(filepath.Join(o.CodexHome, "session_index.jsonl")); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		var b bytes.Buffer
		for scanner.Scan() {
			var v map[string]any
			if json.Unmarshal(scanner.Bytes(), &v) == nil {
				if id, _ := v["id"].(string); selected[id] {
					b.Write(scanner.Bytes())
					b.WriteByte('\n')
				}
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if b.Len() > 0 {
			if err := os.WriteFile(filepath.Join(stage, "native", "home", "session_index.jsonl"), b.Bytes(), 0600); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func snapshotDB(ctx context.Context, source, target string, ids []string, o ExportOptions, stage string, m *Manifest) error {
	src, err := openDB(source, true)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	dst, err := openDB(target, false)
	if err != nil {
		return err
	}
	defer dst.Close()
	tx, err := src.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT name,sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return err
	}
	type schema struct{ name, sql string }
	var tables []schema
	for rows.Next() {
		var s schema
		if err := rows.Scan(&s.name, &s.sql); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, s)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, s := range tables {
		if _, err := dst.ExecContext(ctx, s.sql); err != nil {
			return err
		}
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	for _, s := range tables {
		where := ""
		queryArgs := []any(nil)
		switch s.name {
		case "_sqlx_migrations":
		case "threads":
			where = " WHERE id IN (" + marks + ")"
			queryArgs = args
		case "thread_spawn_edges":
			where = " WHERE parent_thread_id IN (" + marks + ") AND child_thread_id IN (" + marks + ")"
			queryArgs = append(append([]any{}, args...), args...)
		case "thread_sections":
			where = " WHERE id IN (SELECT thread_section_id FROM threads WHERE id IN (" + marks + "))"
			queryArgs = args
		case "projects":
			where = " WHERE id IN (SELECT project_id FROM threads WHERE id IN (" + marks + "))"
			queryArgs = args
		case "project_roots":
			where = " WHERE project_id IN (SELECT project_id FROM threads WHERE id IN (" + marks + "))"
			queryArgs = args
		default:
			columns, err := columnNames(ctx, tx, s.name)
			if err != nil {
				return err
			}
			hasThread := false
			for _, column := range columns {
				if column == "thread_id" {
					hasThread = true
					break
				}
			}
			if !hasThread {
				continue
			}
			where = " WHERE thread_id IN (" + marks + ")"
			queryArgs = args
		}
		rows, err := tx.QueryContext(ctx, "SELECT * FROM "+quote(s.name)+where, queryArgs...)
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return err
		}
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				return err
			}
			if s.name == "queued_items" {
				rows.Close()
				return fmt.Errorf("thread has queued work and cannot be archived")
			}
			for i, v := range values {
				if text, ok := v.(string); s.name == "thread_attachments" && cols[i] == "payload" && ok && json.Valid([]byte(text)) {
					if err := collectAttachments(ctx, []byte(text), o, stage, m); err != nil {
						rows.Close()
						return err
					}
				}
			}
			if _, err := dst.ExecContext(ctx, "INSERT INTO "+quote(s.name)+" VALUES ("+strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")+")", values...); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	// Native secondary indexes contain no data and remain useful on a fresh import.
	rows, err = tx.QueryContext(ctx, "SELECT sql FROM sqlite_master WHERE type IN ('index','trigger') AND sql IS NOT NULL")
	if err != nil {
		return err
	}
	var indexes []string
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			rows.Close()
			return err
		}
		indexes = append(indexes, ddl)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, ddl := range indexes {
		if _, err := dst.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func columnNames(ctx context.Context, db querier, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT * FROM "+quote(table)+" LIMIT 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rows.Columns()
}

func collectAttachments(ctx context.Context, data []byte, o ExportOptions, stage string, m *Manifest) error {
	return collectAttachmentReader(ctx, bytes.NewReader(data), o, stage, m, true)
}
func collectAttachmentReader(ctx context.Context, reader io.Reader, o ExportOptions, stage string, m *Manifest, explicitPayload ...bool) error {
	// These are the native protocol's local file references. Arbitrary strings
	// and tool output are never interpreted as permission to copy host files.
	var visit func(any, string) error
	visit = func(v any, key string) error {
		switch x := v.(type) {
		case map[string]any:
			kind, _ := x["type"].(string)
			local := kind == "local_image" || kind == "input_file" || kind == "ImageView" || kind == "image_view"
			allowPaths := local || (len(explicitPayload) > 0 && explicitPayload[0])
			for k, value := range x {
				if (k == "path" || k == "file_path" || k == "image_path" || k == "local_image_path") && !allowPaths {
					continue
				}
				if err := visit(value, k); err != nil {
					return err
				}
			}
		case []any:
			for _, value := range x {
				if err := visit(value, key); err != nil {
					return err
				}
			}
		case string:
			if key != "local_images" && key != "local_image_path" && key != "image_path" && key != "file_path" && key != "path" {
				return nil
			}
			if !filepath.IsAbs(x) {
				return nil
			}
			resolved, err := filepath.EvalSymlinks(x)
			if err != nil {
				return fmt.Errorf("referenced local attachment is unavailable: %s", x)
			}
			if within(o.ProjectPath, resolved) {
				return nil
			}
			info, err := os.Lstat(resolved)
			if os.IsNotExist(err) {
				return fmt.Errorf("referenced local attachment is unavailable: %s", x)
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("attachment is not a regular file: %s", x)
			}
			// Credentials/configuration can never be smuggled in by a path in a transcript.
			if within(o.CodexHome, filepath.Clean(x)) || within(o.CodexHome, resolved) {
				allowed := false
				for _, sub := range []string{"attachments", "images", "archive_attachments"} {
					allowed = allowed || within(filepath.Join(o.CodexHome, sub), resolved)
				}
				if !allowed {
					return fmt.Errorf("unsupported native file attachment location: %s", x)
				}
			}
			for _, e := range m.Files {
				if e.SourcePath == x {
					return nil
				}
			}
			id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(x)).String()
			rel := filepath.ToSlash(filepath.Join("native", "attachments", id, filepath.Base(x)))
			if err := copyFile(ctx, resolved, filepath.Join(stage, filepath.FromSlash(rel)), info); err != nil {
				return err
			}
			m.Files = append(m.Files, Entry{Path: rel, SourcePath: x})
		}
		return nil
	}
	scanner := bufio.NewScanner(&contextReader{ctx: ctx, r: reader})
	scanner.Buffer(make([]byte, 4096), maxNativeLineBytes)
	for scanner.Scan() {
		var value any
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return fmt.Errorf("invalid native JSON: %w", err)
		}
		if err := visit(value, ""); err != nil {
			return err
		}
	}
	return scanner.Err()
}
