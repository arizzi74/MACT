package sessionarchive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This gate uses fabricated data, isolated HOME/CODEX_HOME and an offline
// provider. The original native store and project are physically deleted
// before Verify starts; no credentials or real sessions are ever accessed.
func TestNativeSelfContainedResumeAcrossRestart(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_ARCHIVE_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_ARCHIVE_TEST=1 for isolated installed-Codex archive proof")
	}
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			o, root := nativeFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			m, err := Export(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(o.CodexHome); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(o.ProjectPath); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(o.CodexHome); !os.IsNotExist(err) {
				t.Fatalf("source native home still available: %v", err)
			}
			verified, err := Verify(ctx, m.ArchivePath, VerifyOptions{CodexBinary: os.Getenv("CODEX_REAL_BINARY")})
			if err != nil {
				t.Fatal(err)
			}
			if !verified.VerifiedSelfContained {
				t.Fatal("proof was not recorded")
			}
			if _, err := os.Stat(filepath.Join(root, "archives", o.ArchiveID)); err != nil {
				t.Fatal("verification removed archive")
			}
		})
	}
}

func TestNativeSelfContainedV2SubagentTreeAcrossRestart(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_ARCHIVE_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_ARCHIVE_TEST=1 for isolated native v2 subagent archive proof")
	}
	for _, mode := range []string{"legacy", "paginated"} {
		t.Run(mode, func(t *testing.T) {
			o, root := nativeFixture(t, mode)
			ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
			defer cancel()
			writeProofConfig(t, o.CodexHome, "http://127.0.0.1:1")
			child, grandchild := uuid.NewString(), uuid.NewString()
			childOptions := o
			childOptions.ProjectPath = filepath.Join(o.ProjectPath, "child")
			grandchildOptions := o
			grandchildOptions.ThreadID = child
			grandchildOptions.ProjectPath = filepath.Join(childOptions.ProjectPath, "nested")
			if err := os.MkdirAll(grandchildOptions.ProjectPath, 0700); err != nil {
				t.Fatal(err)
			}
			writeSubagentFixture(t, childOptions, child, mode)
			writeSubagentFixture(t, grandchildOptions, grandchild, mode)
			client, err := startVerificationServer(ctx, os.Getenv("CODEX_REAL_BINARY"), o.CodexHome, o.ProjectPath, root)
			if err != nil {
				t.Fatal(err)
			}
			// Materialize the exact official schema and metadata with native v1
			// fixtures, then persist the v2 lifecycle marker before export.
			for _, id := range []string{o.ThreadID, child, grandchild} {
				if err := client.call("thread/resume", map[string]any{"threadId": id, "excludeTurns": true, "modelProvider": "archive_verify", "model": "archive-proof"}, nil); err != nil {
					client.close()
					t.Fatal(err)
				}
			}
			client.close()
			for _, id := range []string{child, grandchild} {
				filename := filepath.Join(o.CodexHome, "sessions", "2026", "10", "18", "rollout-2026-10-18T12-00-00-"+id+".jsonl")
				contents, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				scanner := bufio.NewScanner(bytes.NewReader(contents))
				var rewritten bytes.Buffer
				for scanner.Scan() {
					var record map[string]any
					if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
						t.Fatal(err)
					}
					if record["type"] == "session_meta" {
						meta := record["payload"].(map[string]any)
						meta["multi_agent_version"] = "v2"
						meta["session_id"] = o.ThreadID
					}
					if err := json.NewEncoder(&rewritten).Encode(record); err != nil {
						t.Fatal(err)
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, rewritten.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
			db, err := openDB(filepath.Join(o.CodexHome, "state_5.sqlite"), false)
			if err != nil {
				t.Fatal(err)
			}
			for _, edge := range [][2]string{{o.ThreadID, child}, {child, grandchild}} {
				if _, err := db.Exec("INSERT OR REPLACE INTO thread_spawn_edges(parent_thread_id,child_thread_id,status) VALUES(?,?,?)", edge[0], edge[1], "completed"); err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			db.Close()
			client, err = startVerificationServer(ctx, os.Getenv("CODEX_REAL_BINARY"), o.CodexHome, o.ProjectPath, root)
			if err != nil {
				t.Fatal(err)
			}
			if err := client.call("thread/resume", map[string]any{"threadId": o.ThreadID, "excludeTurns": true, "modelProvider": "archive_verify", "model": "archive-proof"}, nil); err != nil {
				client.close()
				t.Fatal(err)
			}
			if err := client.call("thread/resume", map[string]any{"threadId": child, "excludeTurns": true, "modelProvider": "archive_verify", "model": "archive-proof"}, nil); err == nil || !strings.Contains(err.Error(), "multi-agent v2 sub-agent") {
				client.close()
				t.Fatalf("v2 subagent fixture did not reject standalone resume through its native lifecycle rule: %v", err)
			}
			client.close()
			m, err := Export(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.ThreadIDs) != 3 {
				t.Fatalf("nested subagent missing: %#v", m.ThreadIDs)
			}
			if err := os.RemoveAll(o.CodexHome); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(o.ProjectPath); err != nil {
				t.Fatal(err)
			}
			verified, err := Verify(ctx, m.ArchivePath, VerifyOptions{CodexBinary: os.Getenv("CODEX_REAL_BINARY")})
			if err != nil {
				t.Fatal(err)
			}
			if !verified.VerifiedSelfContained || len(verified.ThreadIDs) != 3 {
				t.Fatal("v2 subagent tree was not verified across restart")
			}
			if _, err := os.Stat(m.ArchivePath); err != nil {
				t.Fatal("verification removed archive")
			}
		})
	}
}

func TestNativeContinuedTurnPreservesCompactionToolsAttachmentsAndDescendants(t *testing.T) {
	if os.Getenv("CODEX_NATIVE_ARCHIVE_TEST") != "1" {
		t.Skip("set CODEX_NATIVE_ARCHIVE_TEST=1 for isolated native continued-turn proof")
	}
	for _, scenario := range []string{"legacy", "paginated", "legacy-retained", "paginated-retained"} {
		t.Run(scenario, func(t *testing.T) {
			mode := strings.TrimSuffix(scenario, "-retained")
			retained := strings.HasSuffix(scenario, "-retained")
			o, root := nativeFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			requests := make(chan []byte, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/responses") {
					http.NotFound(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				item := map[string]any{"id": "msg-proof", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Archive continuation verified.", "annotations": []any{}}}}
				for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": "resp-proof", "object": "response", "status": "in_progress", "output": []any{}}}, {"type": "response.output_item.done", "output_index": 0, "item": item}, {"type": "response.completed", "response": map[string]any{"id": "resp-proof", "object": "response", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25}}}} {
					b, _ := json.Marshal(event)
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], b)
				}
			}))
			defer server.Close()
			writeProofConfig(t, o.CodexHome, server.URL)
			ancestor := o.ThreadID
			if mode == "paginated" {
				ancestor = uuid.NewString()
				writeFixtureRollout(t, o.CodexHome, o.ProjectPath, ancestor, mode, nil, o.CodexVersion)
			}
			ancestorPath := nativeFixtureRollout(t, o.CodexHome, ancestor)
			appendCompaction(t, ancestorPath, mode)
			if mode == "paginated" {
				prefix, err := os.ReadFile(ancestorPath)
				if err != nil {
					t.Fatal(err)
				}
				ordinal := bytes.Count(prefix, []byte{'\n'})
				base := map[string]any{"thread_id": ancestor, "end_ordinal_exclusive": ordinal, "end_byte_offset": len(prefix)}
				originalPath := nativeFixtureRollout(t, o.CodexHome, o.ThreadID)
				forkPath := writeFixtureRollout(t, o.CodexHome, o.ProjectPath, o.ThreadID, mode, base, o.CodexVersion)
				if forkPath != originalPath {
					if err := os.Rename(forkPath, originalPath); err != nil {
						t.Fatal(err)
					}
					forkPath = originalPath
				}
				adjustFixtureOrdinals(t, forkPath, ordinal)
				// A continuation after the frozen fork point must neither be bundled
				// nor become part of the resumed child's model-visible context.
				f, err := os.OpenFile(ancestorPath, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				json.NewEncoder(f).Encode(map[string]any{"timestamp": "2026-10-18T12:01:00Z", "ordinal": ordinal, "type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "UNRELATED_ANCESTOR_CONTINUATION_MUST_BE_ABSENT"}}}})
				f.Close()
			}
			child := uuid.NewString()
			writeSubagentFixture(t, o, child, mode)
			client, err := startVerificationServer(ctx, os.Getenv("CODEX_REAL_BINARY"), o.CodexHome, o.ProjectPath, root)
			if err != nil {
				t.Fatal(err)
			}
			initialIDs := []string{o.ThreadID, child}
			if ancestor != o.ThreadID {
				initialIDs = append(initialIDs, ancestor)
			}
			for _, id := range initialIDs {
				if err := client.call("thread/resume", map[string]any{"threadId": id, "excludeTurns": true, "modelProvider": "archive_verify", "model": "archive-proof"}, nil); err != nil {
					client.close()
					t.Fatal(err)
				}
			}
			client.close()
			db, err := openDB(filepath.Join(o.CodexHome, "state_5.sqlite"), false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT OR REPLACE INTO thread_spawn_edges(parent_thread_id,child_thread_id,status) VALUES(?,?,?)", o.ThreadID, child, "completed"); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE threads SET name=? WHERE id=?", "Preserved native name", o.ThreadID); err != nil {
				db.Close()
				t.Fatal(err)
			}
			attachment := filepath.Join(root, "proof-attachment.txt")
			if err := os.WriteFile(attachment, []byte("attachment-content-nonce"), 0600); err != nil {
				db.Close()
				t.Fatal(err)
			}
			payload, _ := json.Marshal(map[string]any{"file_path": attachment})
			if _, err := db.Exec("INSERT INTO thread_attachments(id,thread_id,attachment_type,identity_key,payload,created_at) VALUES(?,?,?,?,?,?)", uuid.NewString(), o.ThreadID, "file", "proof-attachment", string(payload), 1); err != nil {
				db.Close()
				t.Fatal(err)
			}
			var ancestorValues []any
			var ancestorBytes []byte
			if retained && ancestor != o.ThreadID {
				cols, _ := columnNames(ctx, db, "threads")
				ancestorValues, err = scanRow(db.QueryRowContext(ctx, "SELECT * FROM threads WHERE id=?", ancestor), len(cols))
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				ancestorBytes, err = os.ReadFile(ancestorPath)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			db.Close()
			m, err := Export(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.ThreadIDs) != 2 {
				t.Fatalf("descendant missing: %#v", m.ThreadIDs)
			}
			if retained {
				archiveClient, err := startVerificationServer(ctx, os.Getenv("CODEX_REAL_BINARY"), o.CodexHome, o.ProjectPath, root)
				if err != nil {
					t.Fatal(err)
				}
				if err := archiveClient.call("thread/turns/list", map[string]any{"threadId": o.ThreadID, "limit": 1, "itemsView": "notLoaded"}, nil); err != nil {
					archiveClient.close()
					t.Fatal(err)
				}
				if err := archiveClient.call("thread/archive", map[string]any{"threadId": o.ThreadID}, nil); err != nil {
					archiveClient.close()
					t.Fatal(err)
				}
				archiveClient.close()
			} else if err := os.RemoveAll(o.CodexHome); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(o.ProjectPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(attachment); err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(root, "imported-codex")
			project := filepath.Join(root, "imported-project")
			if retained {
				home = o.CodexHome
				project = o.ProjectPath
			}
			if _, err := Restore(ctx, RestoreOptions{ArchivePath: m.ArchivePath, CodexHome: home, Destination: project, CodexVersion: o.CodexVersion}); err != nil {
				t.Fatal(err)
			}
			writeProofConfig(t, home, server.URL)
			db, err = openDB(filepath.Join(home, "state_5.sqlite"), true)
			if err != nil {
				t.Fatal(err)
			}
			var name, storedPayload string
			var edges int
			if err := db.QueryRow("SELECT name FROM threads WHERE id=?", o.ThreadID).Scan(&name); err != nil || name != "Preserved native name" {
				db.Close()
				t.Fatalf("name metadata lost: %q %v", name, err)
			}
			if err := db.QueryRow("SELECT payload FROM thread_attachments WHERE thread_id=?", o.ThreadID).Scan(&storedPayload); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if strings.Contains(storedPayload, attachment) {
				db.Close()
				t.Fatal("attachment reference not rebound")
			}
			var stored struct {
				Path string `json:"file_path"`
			}
			json.Unmarshal([]byte(storedPayload), &stored)
			if b, err := os.ReadFile(stored.Path); err != nil || string(b) != "attachment-content-nonce" {
				db.Close()
				t.Fatalf("attachment payload lost: %q %v", b, err)
			}
			db.QueryRow("SELECT count(*) FROM thread_spawn_edges WHERE parent_thread_id=? AND child_thread_id=?", o.ThreadID, child).Scan(&edges)
			db.Close()
			if edges != 1 {
				t.Fatal("descendant metadata lost")
			}
			for pass := 0; pass < 2; pass++ {
				client, err := startVerificationServer(ctx, os.Getenv("CODEX_REAL_BINARY"), home, project, root)
				if err != nil {
					t.Fatal(err)
				}
				if retained && pass == 0 {
					for _, id := range []string{o.ThreadID, child} {
						if err := client.call("thread/unarchive", map[string]any{"threadId": id}, nil); err != nil {
							client.close()
							t.Fatal(err)
						}
					}
				}
				for _, id := range []string{o.ThreadID, child} {
					if err := client.call("thread/resume", map[string]any{"threadId": id, "excludeTurns": true, "modelProvider": "archive_verify", "model": "archive-proof"}, nil); err != nil {
						client.close()
						t.Fatalf("resume %s: %v", id, err)
					}
				}
				if err := client.call("turn/start", map[string]any{"threadId": o.ThreadID, "input": []any{map[string]any{"type": "text", "text": "Continue the isolated archive proof."}}}, nil); err != nil {
					client.close()
					t.Fatal(err)
				}
				select {
				case body := <-requests:
					for _, nonce := range []string{"compacted-context-nonce", "tool-result-nonce", "Continue the isolated archive proof."} {
						if !bytes.Contains(body, []byte(nonce)) {
							client.close()
							t.Fatalf("model request lost %q after restart pass %d", nonce, pass+1)
						}
					}
					if bytes.Contains(body, []byte("UNRELATED_ANCESTOR_CONTINUATION_MUST_BE_ABSENT")) {
						client.close()
						t.Fatal("fork inherited history beyond its frozen prefix")
					}
				case <-ctx.Done():
					client.close()
					t.Fatal("mock model did not receive a continuation")
				}
				if err := client.waitNotification("turn/completed"); err != nil {
					client.close()
					t.Fatal(err)
				}
				if retained && ancestor != o.ThreadID {
					visible := map[string]bool{}
					for _, archived := range []bool{false, true} {
						var page struct {
							Data []struct {
								ID string `json:"id"`
							} `json:"data"`
						}
						if err := client.call("thread/list", map[string]any{"archived": archived, "limit": 100, "sourceKinds": []string{"cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown"}}, &page); err != nil {
							client.close()
							t.Fatal(err)
						}
						for _, thread := range page.Data {
							visible[thread.ID] = true
						}
					}
					if len(visible) != 3 || !visible[o.ThreadID] || !visible[child] || !visible[ancestor] {
						client.close()
						t.Fatalf("native repair created phantom visible sessions: %#v", visible)
					}
				}
				client.close()
				if retained && ancestor != o.ThreadID {
					current, err := os.ReadFile(ancestorPath)
					if err != nil || !bytes.Equal(current, ancestorBytes) {
						t.Fatalf("independent ancestor rollout changed: %v", err)
					}
					db, err := openDB(filepath.Join(home, "state_5.sqlite"), true)
					if err != nil {
						t.Fatal(err)
					}
					cols, _ := columnNames(ctx, db, "threads")
					values, err := scanRow(db.QueryRowContext(ctx, "SELECT * FROM threads WHERE id=?", ancestor), len(cols))
					db.Close()
					if err != nil || !reflect.DeepEqual(values, ancestorValues) {
						t.Fatalf("independent ancestor metadata changed: %v", err)
					}
				}
			}
		})
	}
}

func writeProofConfig(t *testing.T, home, baseURL string) {
	t.Helper()
	config := fmt.Sprintf("model_provider = \"archive_verify\"\nmodel = \"archive-proof\"\ncheck_for_update_on_startup = false\n[model_providers.archive_verify]\nname = \"Offline proof\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nsupports_websockets = false\n", baseURL)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
}

// The native gate starts the selected real runtime, obtains its own thread ID
// and session metadata, then appends fabricated history for the preservation
// scenarios. Both the manifest version and SQLite schemas come from that exact
// binary, rather than relabelling a 0.163 fixture as a newer archive.
func nativeFixture(t *testing.T, mode string) (ExportOptions, string) {
	t.Helper()
	o, root := fixture(t, mode)
	if err := os.Remove(nativeFixtureRollout(t, o.CodexHome, o.ThreadID)); err != nil {
		t.Fatal(err)
	}
	version := os.Getenv("CODEX_NATIVE_ARCHIVE_VERSION")
	if version == "" {
		version = "0.163.0"
	}
	binary := os.Getenv("CODEX_REAL_BINARY")
	if binary == "" {
		binary = "codex"
	}
	versionCommand := exec.Command(binary, "--version")
	versionCommand.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "CODEX_HOME=" + o.CodexHome, "CODEX_SQLITE_HOME=" + o.CodexHome}
	raw, err := versionCommand.Output()
	if err != nil || nativeVersion(string(raw)) != version {
		t.Fatalf("native proof requires exact Codex %s, got %q: %v", version, raw, err)
	}
	o.CodexVersion = version
	writeProofConfig(t, o.CodexHome, "http://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client, err := startVerificationServer(ctx, binary, o.CodexHome, o.ProjectPath, root)
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := client.call("thread/start", map[string]any{"cwd": o.ProjectPath, "historyMode": mode, "modelProvider": "archive_verify", "model": "archive-proof"}, &started); err != nil {
		client.close()
		t.Fatal(err)
	}
	o.ThreadID = started.Thread.ID
	if err := client.call("thread/name/set", map[string]any{"threadId": o.ThreadID, "name": "Native archive proof"}, nil); err != nil {
		client.close()
		t.Fatal(err)
	}
	client.close()
	filename := nativeFixtureRollout(t, o.CodexHome, o.ThreadID)
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var generated map[string]any
	if err := json.Unmarshal(bytes.SplitN(contents, []byte{'\n'}, 2)[0], &generated); err != nil {
		t.Fatal(err)
	}
	metadata, ok := generated["payload"].(map[string]any)
	if !ok || generated["type"] != "session_meta" || metadata["cli_version"] != version || metadata["id"] != o.ThreadID {
		t.Fatalf("runtime did not generate matching native metadata: %#v", generated)
	}
	records := fixtureRecords(o.ProjectPath, o.ThreadID, mode, nil, version)
	records[0] = generated
	var rewritten bytes.Buffer
	for _, record := range records {
		if err := json.NewEncoder(&rewritten).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filename, rewritten.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return o, root
}

func nativeFixtureRollout(t *testing.T, home, id string) string {
	t.Helper()
	all, err := scanRollouts(t.Context(), home)
	if err != nil || all[id] == nil {
		t.Fatalf("native fixture rollout missing for %s: %v", id, err)
	}
	return all[id].path
}

func appendCompaction(t *testing.T, filename, mode string) {
	t.Helper()
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	ordinal := bytes.Count(original, []byte{'\n'})
	record := map[string]any{"timestamp": "2026-10-18T12:00:00Z", "type": "compacted", "payload": map[string]any{"message": "compacted-context-nonce", "window_number": 1, "window_id": uuid.NewString(), "replacement_history": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "compacted-context-nonce"}}}, map[string]any{"type": "function_call", "name": "exec_command", "call_id": "proof-call", "arguments": "{\"cmd\":\"true\"}"}, map[string]any{"type": "function_call_output", "call_id": "proof-call", "output": "tool-result-nonce"}, map[string]any{"type": "message", "role": "assistant", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": "Compaction retained."}}}}}}
	if mode == "paginated" {
		record["ordinal"] = ordinal
	}
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(record); err != nil {
		t.Fatal(err)
	}
}

func adjustFixtureOrdinals(t *testing.T, filename string, start int) {
	t.Helper()
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(original))
	var b bytes.Buffer
	for scanner.Scan() {
		var v map[string]any
		json.Unmarshal(scanner.Bytes(), &v)
		v["ordinal"] = start
		start++
		json.NewEncoder(&b).Encode(v)
	}
	if err := os.WriteFile(filename, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

func (c *verificationServer) waitNotification(method string) error {
	for c.scanner.Scan() {
		var v struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(c.scanner.Bytes(), &v); err != nil {
			return err
		}
		if v.Method == method {
			return nil
		}
	}
	if err := c.scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("app-server exited before %s", method)
}

func writeSubagentFixture(t *testing.T, o ExportOptions, id, mode string) {
	t.Helper()
	records := fixtureRecords(o.ProjectPath, id, mode, nil, o.CodexVersion)
	meta := records[0]["payload"].(map[string]any)
	meta["session_id"] = o.ThreadID
	meta["parent_thread_id"] = o.ThreadID
	meta["thread_source"] = "subagent"
	meta["source"] = map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": o.ThreadID, "depth": 1}}}
	filename := filepath.Join(o.CodexHome, "sessions", "2026", "10", "18", "rollout-2026-10-18T12-00-00-"+id+".jsonl")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, record := range records {
		if err := json.NewEncoder(f).Encode(record); err != nil {
			t.Fatal(err)
		}
	}
}
