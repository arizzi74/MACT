package registry

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func trustedDeviceLogin(t *testing.T, s *Store, predecessor string, credentialID []byte, choice *bool, name string) (string, AdminBrowserSession) {
	t.Helper()
	c, err := s.NewAdminLoginCeremony(t.Context(), []byte(`{"challenge":"dHJ1c3RlZC1kZXZpY2U"}`), predecessor,
		"Mozilla/5.0 (iPhone) Version/18.0 Safari/604.1", AdminLoginOptions{TrustDevice: choice, DeviceName: name})
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.CompleteAdminLogin(t.Context(), c.ID, c.Binding, adminLoginCredential(credentialID))
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.AdminSessionInfo(t.Context(), token)
	if err != nil {
		t.Fatal(err)
	}
	return token, info
}

func TestAdminTrustedDeviceFixedDeadlineAndPasskeyReauthentication(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	ordinary, credentialID := webPushTestLogin(t, s)
	trust, noTrust := true, false
	token, original := trustedDeviceLogin(t, s, ordinary, credentialID, &trust, "  My iPhone  ")
	if !original.TrustedDevice || original.DeviceName != "My iPhone" || original.BrowserLabel != "Safari on iOS" || original.ExpiresAt.Sub(original.CreatedAt) != 30*24*time.Hour {
		t.Fatalf("trusted session: %+v", original)
	}
	if _, err := s.ValidateAdminSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	// A sensitive action's verified reauthentication updates freshness while
	// retaining the absolute trusted deadline and the revocable device ID.
	if _, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET created_at=$1 WHERE token_hash=$2`, time.Now().Add(-6*time.Minute), hashSecret(token)); err != nil {
		t.Fatal(err)
	}
	renewed, fresh := trustedDeviceLogin(t, s, token, credentialID, nil, "")
	if renewed == token || fresh.ID == original.ID || fresh.DeviceID != original.DeviceID || !fresh.TrustedDevice || fresh.DeviceName != original.DeviceName || !fresh.ExpiresAt.Equal(original.ExpiresAt) || time.Since(fresh.ReauthenticatedAt) > time.Minute {
		t.Fatalf("reauthentication extended trust or lost device: original=%+v fresh=%+v", original, fresh)
	}
	if _, err := s.ValidateAdminSession(ctx, token); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("rotated token valid: %v", err)
	}
	_, untrusted := trustedDeviceLogin(t, s, renewed, credentialID, &noTrust, "")
	if untrusted.TrustedDevice || untrusted.ExpiresAt.Sub(untrusted.CreatedAt) != 8*time.Hour || untrusted.DeviceID != original.DeviceID {
		t.Fatalf("explicit opt-out retained trust: %+v", untrusted)
	}
}

func TestAdminTrustedDeviceExpiryRequiresExplicitNewTrust(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	viewer, credentialID := webPushTestLogin(t, s)
	trust := true
	token, _ := trustedDeviceLogin(t, s, "", credentialID, &trust, "")
	if _, err := s.pool.Exec(ctx, `UPDATE admin_sessions SET expires_at=$1 WHERE token_hash=$2`, time.Now().Add(-time.Second), hashSecret(token)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ValidateAdminSession(ctx, token); !errors.Is(err, ErrAdminSessionInvalid) {
		t.Fatalf("expired trusted login valid: %v", err)
	}
	list, err := s.AdminBrowserSessions(ctx, viewer)
	if err != nil || len(list) != 1 || list[0].TrustedDevice {
		t.Fatalf("expired device remains active: %+v %v", list, err)
	}
	ordinary, info := trustedDeviceLogin(t, s, token, credentialID, nil, "")
	if info.TrustedDevice || info.ExpiresAt.Sub(info.CreatedAt) != 8*time.Hour {
		t.Fatalf("expired trust implicitly renewed: %+v", info)
	}
	_, renewed := trustedDeviceLogin(t, s, ordinary, credentialID, &trust, "")
	if !renewed.TrustedDevice || renewed.ExpiresAt.Sub(renewed.CreatedAt) != 30*24*time.Hour || !renewed.ExpiresAt.After(info.ExpiresAt) {
		t.Fatalf("explicit new trust refused: %+v", renewed)
	}
}

func TestAdminPasswordCannotCarryTrustedDevice(t *testing.T) {
	s := integrationStore(t)
	ctx := t.Context()
	ordinary, credentialID := passwordTestAdmin(t, s)
	passwordTestEnable(t, s, ordinary)
	trust := true
	token, trusted := trustedDeviceLogin(t, s, ordinary, credentialID, &trust, "Shared laptop")
	passwordToken := passwordTestLogin(t, s, token)
	info, err := s.AdminSessionInfo(ctx, passwordToken)
	if err != nil || info.TrustedDevice || info.AuthenticationMethod != "password" || info.ExpiresAt.Sub(info.CreatedAt) != 8*time.Hour || info.DeviceID != trusted.DeviceID {
		t.Fatalf("password retained trusted lifetime: %+v %v", info, err)
	}
	_, passkey := trustedDeviceLogin(t, s, passwordToken, credentialID, nil, "")
	if passkey.TrustedDevice || passkey.ExpiresAt.Sub(passkey.CreatedAt) != 8*time.Hour {
		t.Fatalf("passkey reauthentication restored password's retired trust: %+v", passkey)
	}
}

func TestAdminDeviceRevocationFencesRotationAndCleansCapabilities(t *testing.T) {
	for _, order := range []string{"revoke-first", "rotate-first", "concurrent", "device-id"} {
		t.Run(order, func(t *testing.T) {
			s := integrationStore(t)
			ctx := context.Background()
			viewer, credentialID := webPushTestLogin(t, s)
			trust := true
			token, info := trustedDeviceLogin(t, s, "", credentialID, &trust, "Work browser")
			webPushTestRegister(t, s, token)
			draftID := uuid.New()
			if _, err := s.PutAdminDraft(ctx, token, draftID, 1, draftCiphertext('t')); err != nil {
				t.Fatal(err)
			}
			ceremony := adminLoginCeremony(t, s, token)
			target := info.ID
			if order == "device-id" {
				target = info.DeviceID
			}
			var rotated string
			var rotateErr, revokeErr error
			rotate := func() {
				rotated, rotateErr = s.CompleteAdminLogin(ctx, ceremony.ID, ceremony.Binding, adminLoginCredential(credentialID))
			}
			revoke := func() { revokeErr = s.RevokeAdminBrowserSessions(ctx, viewer, &target) }
			switch order {
			case "revoke-first":
				revoke()
				rotate()
			case "rotate-first", "device-id":
				rotate()
				revoke()
			case "concurrent":
				var wg sync.WaitGroup
				start := make(chan struct{})
				wg.Add(2)
				go func() { defer wg.Done(); <-start; rotate() }()
				go func() { defer wg.Done(); <-start; revoke() }()
				close(start)
				wg.Wait()
			}
			if revokeErr != nil || rotateErr != nil && !errors.Is(rotateErr, ErrAdminSessionInvalid) {
				t.Fatalf("revoke=%v rotation=%v", revokeErr, rotateErr)
			}
			for _, candidate := range []string{token, rotated} {
				if _, err := s.ValidateAdminSession(ctx, candidate); !errors.Is(err, ErrAdminSessionInvalid) {
					t.Fatalf("revoked device token remains valid: %v", err)
				}
			}
			if _, err := s.ValidateAdminSession(ctx, viewer); err != nil {
				t.Fatal("other device revoked", err)
			}
			if webPushCount(t, s, "webpush_subscriptions") != 0 {
				t.Fatal("device revocation retained push subscription")
			}
			if _, err := s.GetAdminDraft(ctx, viewer, draftID); !errors.Is(err, ErrAdminDraftNotFound) {
				t.Fatalf("device revocation retained draft: %v", err)
			}
		})
	}
}

func TestAdminDeviceNamesAreBoundedPlainText(t *testing.T) {
	s := integrationStore(t)
	for _, name := range []string{strings.Repeat("x", 81), strings.Repeat("📱", 81), "bad\nname", "bad\x00name", "<script>name</script>", string([]byte{0xff})} {
		if _, err := s.NewAdminLoginCeremony(t.Context(), []byte(`{"challenge":"dHJ1c3RlZA"}`), "", "", AdminLoginOptions{DeviceName: name}); !errors.Is(err, ErrAdminDeviceNameInvalid) {
			t.Fatalf("unsafe device name %q accepted: %v", name, err)
		}
	}
	if name, valid := normalizeAdminDeviceName("  " + strings.Repeat("è", 80) + "  "); !valid || name != strings.Repeat("è", 80) {
		t.Fatal("bounded Unicode name rejected")
	}
}
