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

func TestRepositoryRenameFetchKeepsCanonicalDownloadsAndHistoricalIdentity(t *testing.T) {
	for _, tc := range []struct{ name, requested, canonical, source, pin string }{
		{"before rename latest", LegacyRepo, LegacyRepo, LegacyRepo, ""},
		{"before rename pinned", LegacyRepo, LegacyRepo, LegacyRepo, "v1.2.3"},
		{"renamed latest", LegacyRepo, RenamedRepo, RenamedRepo, ""},
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
				case "https://api.github.com/repos/" + tc.requested + "/releases/latest", "https://api.github.com/repos/" + tc.requested + "/releases/tags/" + tag:
					data, _ = json.Marshal(map[string]any{"tag_name": tag, "draft": false, "prerelease": false, "html_url": "https://github.com/" + tc.canonical + "/releases/tag/" + tag, "assets": []map[string]string{
						{"name": "SHA256SUMS", "browser_download_url": prefix + "SHA256SUMS"},
						{"name": "SHA256SUMS.sigstore.json", "browser_download_url": prefix + "SHA256SUMS.sigstore.json"},
					}})
				case "https://api.github.com/repos/" + LegacyRepo:
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
			if release.Repo != tc.canonical || release.SourceRepo != tc.source || verifications != 1 {
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

func TestRepositoryRenameRejectsOtherRepositoriesAndChangedIdentity(t *testing.T) {
	for _, tc := range []struct{ name, requested, canonical, repositoryJSON, assetRepo string }{
		{"arbitrary destination", LegacyRepo, "attacker/project", "", "attacker/project"},
		{"fork cannot migrate", "owner/fork", RenamedRepo, "", RenamedRepo},
		{"new cannot migrate backwards", RenamedRepo, LegacyRepo, "", LegacyRepo},
		{"wrong immutable repository", LegacyRepo, RenamedRepo, `{"id":1,"full_name":"arizzi74/MACT"}`, RenamedRepo},
		{"wrong canonical repository", LegacyRepo, RenamedRepo, fmt.Sprintf(`{"id":%d,"full_name":"attacker/project"}`, officialRepositoryID), RenamedRepo},
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
				if request.URL.String() == "https://api.github.com/repos/"+LegacyRepo {
					return coreResponse(request, []byte(tc.repositoryJSON)), nil
				}
				if request.URL.Host != "api.github.com" {
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
	for _, source := range []string{LegacyRepo, RenamedRepo, "attacker/project"} {
		t.Run(source, func(t *testing.T) {
			m := New(nil)
			checks := 0
			m.verifyManifest = func(repo, tag string, manifest, attestation []byte) error {
				checks++
				return errors.New("invalid signature")
			}
			m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
				prefix := "https://github.com/" + RenamedRepo + "/releases/download/v1.2.3/"
				var data []byte
				switch {
				case request.URL.Host == "api.github.com":
					data, _ = json.Marshal(map[string]any{"tag_name": "v1.2.3", "draft": false, "prerelease": false, "assets": []map[string]string{{"name": "SHA256SUMS", "browser_download_url": prefix + "SHA256SUMS"}, {"name": "SHA256SUMS.sigstore.json", "browser_download_url": prefix + "SHA256SUMS.sigstore.json"}}})
				case strings.HasSuffix(request.URL.Path, "/SHA256SUMS"):
					data = []byte(coreHash(nil) + "  program\n")
				case strings.HasSuffix(request.URL.Path, "/SHA256SUMS.sigstore.json"):
					data = repositoryBundle(t, source, "v1.2.3")
				default:
					t.Fatal("unauthenticated package download", request.URL)
				}
				return coreResponse(request, data), nil
			})
			if _, err := m.FetchRelease(t.Context(), RenamedRepo, ""); err == nil {
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

func TestRepositoryRenameCommitsOnlyAfterSuccessfulReadiness(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			l, packages, release := updateFixture(t, "worker")
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
				release.Repo, release.Tag = RenamedRepo, "v0.6.21"
				if err := WriteJSON(l.State, map[string]any{"schema": 1, "component": "worker", "repo": LegacyRepo, "version": release.Tag, "pending": pending}); err != nil {
					t.Fatal(err)
				}
				m := &Manager{ReadyTimeout: 10 * time.Millisecond, PollInterval: time.Millisecond, Run: func(_ context.Context, args ...string) (CommandResult, error) {
					if len(args) == 2 && args[1] == "version" {
						return CommandResult{Output: []byte(release.Tag)}, nil
					}
					if len(args) == 4 && args[3] == "status" {
						data, _ := json.Marshal(map[string]any{"updated_at": time.Now().Add(time.Second), "gateway_connected": ready, "version": release.Tag, "runtimes": []any{}})
						return CommandResult{Output: data}, nil
					}
					t.Fatal("same-version queued update mutated service", args)
					return CommandResult{}, nil
				}}
				_, err := m.requestedWorkerReleaseStep(t.Context(), l, &requestedWorkerPlan{release: release})
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
		name                                               string
		check, pending, ready, invalidSignature, prebridge bool
	}{
		{name: "read only check", check: true},
		{name: "successful same version"},
		{name: "pending not ready", pending: true},
		{name: "pending ready", pending: true, ready: true},
		{name: "invalid signature", invalidSignature: true},
		{name: "prebridge same version", prebridge: true},
		{name: "prebridge read only check", prebridge: true, check: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, l := coreWorkerHome(t)
			tag := "v0.6.21"
			if tc.prebridge {
				tag = "v0.6.20"
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
				if tc.prebridge && !tc.check {
					t.Fatal("prebridge same-version update reached installed commands")
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
				if repo != LegacyRepo || signedTag != tag {
					t.Fatal("wrong bridge signing identity", repo, signedTag)
				}
				if tc.invalidSignature {
					return errors.New("invalid signature")
				}
				return nil
			}
			bundle := repositoryBundle(t, LegacyRepo, tag)
			prefix := "https://github.com/" + RenamedRepo + "/releases/download/" + tag + "/"
			m.HTTP.Transport = coreRoundTripper(func(request *http.Request) (*http.Response, error) {
				var data []byte
				switch request.URL.String() {
				case "https://api.github.com/repos/" + LegacyRepo + "/releases/latest":
					data, _ = json.Marshal(map[string]any{"tag_name": tag, "html_url": "https://github.com/" + RenamedRepo + "/releases/tag/" + tag, "draft": false, "prerelease": false, "assets": []map[string]string{{"name": "SHA256SUMS", "browser_download_url": prefix + "SHA256SUMS"}, {"name": "SHA256SUMS.sigstore.json", "browser_download_url": prefix + "SHA256SUMS.sigstore.json"}}})
				case "https://api.github.com/repos/" + LegacyRepo:
					data = []byte(fmt.Sprintf(`{"id":%d,"full_name":%q}`, officialRepositoryID, RenamedRepo))
				case prefix + "SHA256SUMS":
					data = []byte(coreHash(nil) + "  program\n")
				case prefix + "SHA256SUMS.sigstore.json":
					data = bundle
				default:
					t.Fatal("unexpected network request", request.URL)
				}
				return coreResponse(request, data), nil
			})
			err := m.execute(t.Context(), options{Action: "update", Component: "worker", Check: tc.check})
			wantSuccess := !tc.invalidSignature && (!tc.pending || tc.ready) && (!tc.prebridge || tc.check)
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
			if tc.prebridge && !tc.check {
				if updateRead(t, l.State) != before || FileExists(l.AliasCommand) {
					t.Fatal("prebridge same-version update mutated state or alias")
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
