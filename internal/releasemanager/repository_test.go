package releasemanager

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// This certificate is deliberately untrusted. Fetch tests replace only the final
// signature verification, allowing them to exercise real identity selection.
func repositoryBundle(t *testing.T, repo, tag string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := url.Parse("https://github.com/" + repo + "/.github/workflows/release.yml@refs/tags/" + tag)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), URIs: []*url.URL{identity}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	encoded, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"certificate":{"rawBytes":"` + base64.StdEncoding.EncodeToString(encoded) + `"}},"dsseEnvelope":{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[{"sig":"AA=="}]}}`)
}

func canonicalRepositoryTransport(t *testing.T, tag, source string) coreRoundTripper {
	t.Helper()
	bundle := repositoryBundle(t, source, tag)
	prefix := "https://github.com/" + RenamedRepo + "/releases/download/" + tag + "/"
	return func(request *http.Request) (*http.Response, error) {
		var data []byte
		switch request.URL.String() {
		case "https://api.github.com/repos/" + RenamedRepo:
			data = []byte(fmt.Sprintf(`{"id":%d,"full_name":%q}`, officialRepositoryID, RenamedRepo))
		case "https://api.github.com/repos/" + RenamedRepo + "/releases/latest":
			data, _ = json.Marshal(map[string]any{"tag_name": tag, "html_url": "https://github.com/" + RenamedRepo + "/releases/tag/" + tag, "draft": false, "prerelease": false, "assets": []map[string]string{{"name": "SHA256SUMS", "browser_download_url": prefix + "SHA256SUMS"}, {"name": "SHA256SUMS.sigstore.json", "browser_download_url": prefix + "SHA256SUMS.sigstore.json"}}})
		case prefix + "SHA256SUMS":
			data = []byte(coreHash(nil) + "  program\n")
		case prefix + "SHA256SUMS.sigstore.json":
			data = bundle
		default:
			t.Fatal("unexpected request, including any request to the recreated legacy repository", request.URL)
		}
		return coreResponse(request, data), nil
	}
}

func TestDefaultRepositoryUsesCanonicalMACTIdentity(t *testing.T) {
	if DefaultRepo != RenamedRepo || LegacyRepo != "arizzi74/Codex-Telegram-Gateway" {
		t.Fatal("default or historical repository identity changed", DefaultRepo, LegacyRepo)
	}
}

func TestRepositoryRenameFetchKeepsCanonicalDownloadsAndHistoricalIdentity(t *testing.T) {
	for _, tc := range []struct{ name, requested, canonical, source, pin string }{
		{"legacy selection ignores recreated repository", LegacyRepo, RenamedRepo, RenamedRepo, ""},
		{"legacy pinned selection ignores recreated repository", LegacyRepo, RenamedRepo, RenamedRepo, "v0.6.23"},
		{"renamed historical pin", LegacyRepo, RenamedRepo, LegacyRepo, "v1.2.3"},
		{"renamed bridge release pin", LegacyRepo, RenamedRepo, LegacyRepo, "v0.6.21"},
		{"persisted canonical latest", RenamedRepo, RenamedRepo, RenamedRepo, ""},
		{"persisted canonical historical pin", RenamedRepo, RenamedRepo, LegacyRepo, "v1.2.3"},
		{"fork independent", "owner/fork", "owner/fork", "owner/fork", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tag := "v1.2.3"
			if tc.pin != "" {
				tag = tc.pin
			}
			bundle := repositoryBundle(t, tc.source, tag)
			m := New(nil)
			verifications, repositoryLookups := 0, 0
			m.verifyManifest = func(repo, signedTag string, manifest, attestation []byte) error {
				verifications++
				if repo != tc.source || signedTag != tag || string(attestation) != string(bundle) || len(manifest) == 0 {
					t.Fatal("incorrect exact provenance policy", repo, signedTag)
				}
				return nil
			}
			m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
				var data []byte
				prefix := "https://github.com/" + tc.canonical + "/releases/download/" + tag + "/"
				switch request.URL.String() {
				case "https://api.github.com/repos/" + tc.canonical + "/releases/latest", "https://api.github.com/repos/" + tc.canonical + "/releases/tags/" + tag:
					if tc.requested == LegacyRepo && repositoryLookups != 1 {
						t.Fatal("legacy selection fetched metadata before confirming the canonical identity")
					}
					data, _ = json.Marshal(map[string]any{"tag_name": tag, "draft": false, "prerelease": false, "html_url": "https://github.com/" + tc.canonical + "/releases/tag/" + tag, "assets": []map[string]string{
						{"name": "SHA256SUMS", "browser_download_url": prefix + "SHA256SUMS"},
						{"name": "SHA256SUMS.sigstore.json", "browser_download_url": prefix + "SHA256SUMS.sigstore.json"},
					}})
				case "https://api.github.com/repos/" + RenamedRepo:
					repositoryLookups++
					data = []byte(fmt.Sprintf(`{"id":%d,"full_name":%q}`, officialRepositoryID, RenamedRepo))
				case prefix + "SHA256SUMS":
					data = []byte(coreHash(nil) + "  codex-worker-linux-amd64.tar.gz\n")
				case prefix + "SHA256SUMS.sigstore.json":
					data = bundle
				default:
					t.Fatalf("unexpected or unscoped download: %s", request.URL)
				}
				return coreResponse(request, data), nil
			})
			release, err := m.FetchRelease(t.Context(), tc.requested, tc.pin)
			if err != nil {
				t.Fatal(err)
			}
			if release.Repo != tc.canonical || release.SourceRepo != tc.source || release.RequestedRepo != tc.requested || verifications != 1 {
				t.Fatalf("unexpected release identity: %+v checks=%d", release, verifications)
			}
			wantLookups := 0
			if tc.requested == LegacyRepo && tc.canonical == RenamedRepo {
				wantLookups = 1
			}
			if repositoryLookups != wantLookups {
				t.Fatalf("repository lookups=%d, want %d", repositoryLookups, wantLookups)
			}
		})
	}
}

func TestLegacyRepositoryCannotFallBackWhenCanonicalIdentityIsUnavailable(t *testing.T) {
	m := New(nil)
	calls := 0
	m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://api.github.com/repos/"+RenamedRepo {
			t.Fatal("canonical identity failure caused a release or legacy repository request", request.URL)
		}
		response := coreResponse(request, []byte(`{"message":"Not Found"}`))
		response.StatusCode = http.StatusNotFound
		return response, nil
	})
	if _, err := m.FetchRelease(t.Context(), LegacyRepo, ""); err == nil || calls != 1 {
		t.Fatalf("canonical identity failure did not stop resolution: calls=%d error=%v", calls, err)
	}
}

func TestRepositoryRenameRejectsOtherRepositoriesAndChangedIdentity(t *testing.T) {
	for _, tc := range []struct{ name, requested, canonical, repositoryJSON, assetRepo string }{
		{"arbitrary destination", LegacyRepo, "attacker/project", "", "attacker/project"},
		{"recreated legacy release metadata", LegacyRepo, LegacyRepo, "", LegacyRepo},
		{"fork cannot migrate", "owner/fork", RenamedRepo, "", RenamedRepo},
		{"new cannot migrate backwards", RenamedRepo, LegacyRepo, "", LegacyRepo},
		{"wrong immutable repository", LegacyRepo, RenamedRepo, `{"id":1,"full_name":"arizzi74/MACT"}`, RenamedRepo},
		{"wrong canonical repository", LegacyRepo, RenamedRepo, fmt.Sprintf(`{"id":%d,"full_name":"attacker/project"}`, officialRepositoryID), RenamedRepo},
		{"malformed canonical identity", LegacyRepo, RenamedRepo, `{`, RenamedRepo},
		{"missing canonical identity", LegacyRepo, RenamedRepo, `{}`, RenamedRepo},
		{"wrong asset repository", LegacyRepo, RenamedRepo, fmt.Sprintf(`{"id":%d,"full_name":"arizzi74/MACT"}`, officialRepositoryID), "attacker/project"},
		{"mixed old assets", LegacyRepo, RenamedRepo, fmt.Sprintf(`{"id":%d,"full_name":"arizzi74/MACT"}`, officialRepositoryID), LegacyRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(nil)
			m.verifyManifest = func(string, string, []byte, []byte) error {
				t.Fatal("invalid metadata reached signature verifier")
				return nil
			}
			m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() == "https://api.github.com/repos/"+RenamedRepo {
					identity := tc.repositoryJSON
					if identity == "" {
						identity = fmt.Sprintf(`{"id":%d,"full_name":%q}`, officialRepositoryID, RenamedRepo)
					}
					return coreResponse(request, []byte(identity)), nil
				}
				selected := tc.requested
				if selected == LegacyRepo {
					selected = RenamedRepo
				}
				if request.URL.String() != "https://api.github.com/repos/"+selected+"/releases/latest" {
					t.Fatal("invalid metadata caused asset download", request.URL)
				}
				data, _ := json.Marshal(map[string]any{"tag_name": "v1.2.3", "draft": false, "prerelease": false, "html_url": "https://github.com/" + tc.canonical + "/releases/tag/v1.2.3", "assets": []map[string]string{{"name": "SHA256SUMS", "browser_download_url": "https://github.com/" + tc.assetRepo + "/releases/download/v1.2.3/SHA256SUMS"}}})
				return coreResponse(request, data), nil
			})
			if _, err := m.FetchRelease(t.Context(), tc.requested, ""); err == nil {
				t.Fatal("accepted unapproved repository migration")
			}
		})
	}
}

func TestRenamedRepositoryNeverFallsBackAfterSignatureFailure(t *testing.T) {
	for _, requested := range []string{LegacyRepo, RenamedRepo} {
		for _, source := range []string{LegacyRepo, RenamedRepo, "attacker/project"} {
			t.Run(requested+"/"+source, func(t *testing.T) {
				m := New(nil)
				checks := 0
				m.verifyManifest = func(repo, tag string, manifest, attestation []byte) error {
					checks++
					return errors.New("invalid signature")
				}
				m.HTTP.Transport = canonicalRepositoryTransport(t, "v1.2.3", source)
				if _, err := m.FetchRelease(t.Context(), requested, ""); err == nil {
					t.Fatal("accepted invalid signature")
				}
				want := 1
				if source == "attacker/project" {
					want = 0
				}
				if checks != want {
					t.Fatalf("verifier calls=%d, want %d; fallback must never occur", checks, want)
				}
			})
		}
	}
}

func TestRepositoryRenameCommitsOnlyAfterSuccessfulReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			l, packages, release := updateFixture(t, "worker")
			if err := SaveSettings(l, LegacyRepo, "v1.0.0"); err != nil {
				t.Fatal(err)
			}
			release.Repo = RenamedRepo
			started := false
			m := &Manager{ReadyTimeout: 10 * time.Millisecond, PollInterval: time.Millisecond, Run: func(_ context.Context, args ...string) (CommandResult, error) {
				if args[0] == "systemctl" {
					if args[2] == "show" {
						return CommandResult{Output: []byte("MainPID=0\nActiveState=inactive\n")}, nil
					}
					if args[2] == "start" {
						started = true
					}
					return CommandResult{}, nil
				}
				if args[0] == l.Binary && args[3] == "status" {
					if !started {
						return CommandResult{ExitCode: 1}, nil
					}
					data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": ready, "version": "1.2.3", "runtimes": []any{}})
					return CommandResult{Output: data}, nil
				}
				t.Fatal("unexpected command", args)
				return CommandResult{}, nil
			}}
			err := m.ApplyUpdate(t.Context(), l, packages, release, false)
			if (err == nil) != ready {
				t.Fatalf("readiness=%v error=%v", ready, err)
			}
			settings, err := SavedSettings(l)
			if err != nil {
				t.Fatal(err)
			}
			want := LegacyRepo
			if ready {
				want = RenamedRepo
			}
			if settings["repo"] != want || (settings["pending"] == true) == ready {
				t.Fatalf("rename committed at wrong readiness boundary: %+v", settings)
			}
		})
	}
}

func TestRenamedRepositoryRejectsPrebridgeInstallBeforeMutation(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprint(fresh), func(t *testing.T) {
			l, packages, release := updateFixture(t, "worker")
			release.Repo, release.Tag = RenamedRepo, "v0.6.20"
			before := updateRead(t, l.State)
			m := &Manager{Run: func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("unsupported historic installation mutated service")
				return CommandResult{}, nil
			}}
			var err error
			if fresh {
				err = m.FreshInstall(t.Context(), l, "", "", packages, release)
			} else {
				err = m.ApplyUpdate(t.Context(), l, packages, release, false)
			}
			if err == nil || !strings.Contains(err.Error(), "install v0.6.21 or newer") {
				t.Fatalf("missing actionable prebridge rejection: %v", err)
			}
			if updateRead(t, l.State) != before || updateRead(t, l.Binary) != "old binary" || updateRead(t, l.Manager) != "old manager" || FileExists(l.Backups) {
				t.Fatal("prebridge rejection mutated installed files")
			}
		})
	}
	for _, repo := range []string{LegacyRepo, "owner/fork"} {
		if err := validateRepositoryBridgeInstall(&Release{Repo: repo, Tag: "v0.6.20"}); err != nil {
			t.Fatalf("prebridge guard leaked into unrenamed or fork policy: %s %v", repo, err)
		}
	}
}

func TestRequestedWorkerCanonicalRepoMigrationWaitsForReadiness(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, ready := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending=%v/ready=%v", pending, ready), func(t *testing.T) {
				l, _, release := updateFixture(t, "worker")
				release.Repo, release.Tag = RenamedRepo, "v0.6.23"
				if err := WriteJSON(l.State, map[string]any{"schema": 1, "component": "worker", "repo": LegacyRepo, "version": release.Tag, "pending": pending}); err != nil {
					t.Fatal(err)
				}
				m := New(nil)
				m.ReadyTimeout, m.PollInterval = 10*time.Millisecond, time.Millisecond
				m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
					if len(args) == 2 && args[1] == "version" {
						return CommandResult{Output: []byte(release.Tag)}, nil
					}
					if len(args) == 4 && args[3] == "status" {
						data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": ready, "version": release.Tag, "runtimes": []any{}})
						return CommandResult{Output: data}, nil
					}
					t.Fatal("same-version queued update mutated service", args)
					return CommandResult{}, nil
				}
				m.HTTP.Transport = canonicalRepositoryTransport(t, release.Tag, RenamedRepo)
				m.verifyManifest = func(repo, tag string, manifest, attestation []byte) error {
					if repo != RenamedRepo || tag != release.Tag {
						t.Fatal("queued update selected wrong canonical provenance", repo, tag)
					}
					return nil
				}
				_, err := m.requestedWorkerReleaseStep(t.Context(), l, &requestedWorkerPlan{})
				wantSuccess := !pending || ready
				if (err == nil) != wantSuccess {
					t.Fatal("unexpected queued readiness result", err)
				}
				settings, err := SavedSettings(l)
				if err != nil {
					t.Fatal(err)
				}
				want := LegacyRepo
				if wantSuccess {
					want = RenamedRepo
				}
				if settings["repo"] != want {
					t.Fatalf("queued migration committed at wrong boundary: %+v", settings)
				}
			})
		}
	}
}

func TestSameVersionCanonicalRepoMigrationHonorsCheckAndPendingReadiness(t *testing.T) {
	for _, tc := range []struct {
		name                                                                string
		check, pending, ready, invalidSignature, invalidIdentity, prebridge bool
	}{
		{name: "read only check", check: true},
		{name: "successful same version"},
		{name: "pending not ready", pending: true},
		{name: "pending ready", pending: true, ready: true},
		{name: "invalid signature", invalidSignature: true},
		{name: "invalid canonical identity", invalidIdentity: true},
		{name: "prebridge same version", prebridge: true},
		{name: "prebridge read only check", prebridge: true, check: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, l := coreWorkerHome(t)
			tag, source := "v0.6.23", RenamedRepo
			if tc.prebridge {
				tag, source = "v0.6.20", LegacyRepo
			}
			if err := AtomicWrite(l.Manager, []byte("bridge manager"), 0755, nil); err != nil {
				t.Fatal(err)
			}
			if err := WriteJSON(l.State, map[string]any{"schema": 1, "component": "worker", "repo": LegacyRepo, "version": tag, "pending": tc.pending}); err != nil {
				t.Fatal(err)
			}
			before := updateRead(t, l.State)
			m.ReadyTimeout, m.PollInterval = 10*time.Millisecond, time.Millisecond
			m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
				if (tc.prebridge && !tc.check) || tc.invalidIdentity {
					t.Fatal("unapproved same-version update reached installed commands")
				}
				if len(args) == 2 && args[0] == l.Binary && args[1] == "version" {
					return CommandResult{Output: []byte(tag)}, nil
				}
				if len(args) == 4 && args[3] == "status" {
					data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": tc.ready, "version": tag, "runtimes": []any{}})
					return CommandResult{Output: data}, nil
				}
				t.Fatal("same-version update mutated worker service", args)
				return CommandResult{}, nil
			}
			m.verifyManifest = func(repo, signedTag string, manifest, attestation []byte) error {
				if tc.invalidIdentity {
					t.Fatal("invalid canonical identity reached signature verification")
				}
				if repo != source || signedTag != tag {
					t.Fatal("wrong bridge signing identity", repo, signedTag)
				}
				if tc.invalidSignature {
					return errors.New("invalid signature")
				}
				return nil
			}
			transport := canonicalRepositoryTransport(t, tag, source)
			m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
				if tc.invalidIdentity && request.URL.String() == "https://api.github.com/repos/"+RenamedRepo {
					return coreResponse(request, []byte(`{"id":1,"full_name":"arizzi74/MACT"}`)), nil
				}
				return transport(request)
			})
			err := m.execute(t.Context(), options{Action: "update", Component: "worker", Check: tc.check})
			wantSuccess := !tc.invalidSignature && !tc.invalidIdentity && (!tc.pending || tc.ready) && (!tc.prebridge || tc.check)
			if (err == nil) != wantSuccess {
				t.Fatalf("unexpected same-version result: %v", err)
			}
			settings, err := SavedSettings(l)
			if err != nil {
				t.Fatal(err)
			}
			want := LegacyRepo
			if wantSuccess && !tc.check {
				want = RenamedRepo
			}
			if settings["repo"] != want {
				t.Fatalf("same-version migration saved wrong repo: %+v", settings)
			}
			if (tc.prebridge && !tc.check) || tc.invalidIdentity {
				if updateRead(t, l.State) != before || FileExists(l.AliasCommand) {
					t.Fatal("unapproved same-version update mutated state or alias")
				}
			}
			if tc.check {
				if updateRead(t, l.State) != before {
					t.Fatal("check mutated persisted state")
				}
				if _, err := os.Lstat(l.AliasCommand); !os.IsNotExist(err) {
					t.Fatal("check installed alias")
				}
			}
		})
	}
}
