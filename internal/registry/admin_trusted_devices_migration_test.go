package registry

import (
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/migrations"
)

func TestAdminTrustedDeviceMigrationPreservesOrdinarySessionAndCeremony(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "legacy-gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `CREATE TABLE schema_migrations (
 version INTEGER NOT NULL PRIMARY KEY, checksum BLOB NOT NULL,
 applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%f', 'now') || '000000Z')
 ) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, file := range migrations.Files {
		version, err := migrationVersion(file)
		if err != nil {
			t.Fatal(err)
		}
		if version > 27 {
			continue
		}
		migration, err := migrations.SQL.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("legacy schema %s: %v", file, err)
		}
		checksum := sha256.Sum256(migration)
		if _, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, version, checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	credentialID := []byte("legacy-passkey")
	token, binding := "legacy-token", "legacy-ceremony-binding"
	sessionID, ceremonyID := uuid.New(), uuid.New()
	created := time.Now().UTC().Add(-time.Hour)
	expires := created.Add(8 * time.Hour)
	if _, err = tx.Exec(ctx, `INSERT INTO admin_credentials(credential_id,user_handle,public_key,credential_json)
 VALUES($1,$2,$1,'{"authenticator":{"signCount":1}}')`, credentialID, []byte("legacy-owner")); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_sessions(session_id,credential_id,token_hash,created_at,expires_at,browser_label)
 VALUES($1,$2,$3,$4,$5,'Safari on iOS')`, sessionID, credentialID, hashSecret(token), created, expires); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO admin_challenges(challenge_id,purpose,challenge_hash,session_data,ceremony_binding_hash,predecessor_token_hash,browser_label,expires_at)
 VALUES($1,'authentication',$2,'{"challenge":"bGVnYWN5"}',$3,$4,'Safari on iOS',$5)`, ceremonyID, hashSecret("legacy-challenge"), hashSecret(binding), hashSecret(token), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := s.AdminSessionInfo(ctx, token)
	if err != nil || info.TrustedDevice || info.DeviceID != sessionID || info.ID != sessionID || !info.CreatedAt.Equal(created) || !info.ExpiresAt.Equal(expires) || info.BrowserLabel != "Safari on iOS" {
		t.Fatalf("upgrade changed ordinary session: %+v %v", info, err)
	}
	renewed, err := s.CompleteAdminLogin(ctx, ceremonyID, binding, adminLoginCredential(credentialID))
	if err != nil {
		t.Fatal("upgrade broke outstanding ceremony", err)
	}
	info, err = s.AdminSessionInfo(ctx, renewed)
	if err != nil || info.TrustedDevice || info.ExpiresAt.Sub(info.CreatedAt) != 8*time.Hour || info.DeviceID != sessionID {
		t.Fatalf("legacy ceremony implicitly granted trust: %+v %v", info, err)
	}
	if err = s.CheckMigrations(ctx); err != nil {
		t.Fatal(err)
	}
}
