// Native archive compatibility is deliberately limited to the exact validated
// Codex 0.163.0 layout. SQLite schemas are fingerprinted; no global database or
// authentication file is copied. Legacy and paginated JSONL are canonical;
// thread_history_1.sqlite contains rebuildable projections and is regenerated.
// Fork source rollouts are frozen at referenced prefix boundaries. Filesystem
// snapshots preserve inert symlinks without following them and reject special
// files. Unsupported history records larger than 32 MiB or manifests larger
// than 16 MiB fail without removing source data.
//
// Restore never replaces a differing project or native history. Interrupted
// imports retain digest-bound intents and reconcile exact published records.
// Retained, archived native records with external attachments use preimage
// checked replacements into managed archive_attachments storage; independent
// ancestors receive private frozen generation copies. Their small repair
// recipes remain available for restart reconciliation.
//
// Offline native acceptance gate (uses only synthetic sessions and a local
// Responses provider, with isolated HOME/CODEX_HOME and no credentials):
//
//	CODEX_NATIVE_ARCHIVE_TEST=1 CODEX_REAL_BINARY=/path/to/codex go test ./internal/sessionarchive -run '^TestNative' -count=1
package sessionarchive
