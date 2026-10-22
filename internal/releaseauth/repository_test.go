package releaseauth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"os"
	"testing"
	"time"
)

// These public release assets were downloaded from the official v0.6.20 tag
// before its repository rename. This test requires no network and proves the
// production parser and verifier preserve the actual historical certificate.
func TestOfficialHistoricalTagRetainsExactLegacyProvenance(t *testing.T) {
	const legacy = "arizzi74/Codex-Telegram-Gateway"
	const renamed = "arizzi74/MACT"
	manifest, err := os.ReadFile("testdata/official-v0.6.20.SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile("testdata/official-v0.6.20.sigstore.json")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := CandidateRepository("v0.6.20", encoded, renamed, legacy)
	if err != nil || repo != legacy {
		t.Fatalf("historical certificate candidate=%q error=%v", repo, err)
	}
	if err := Verify(repo, "v0.6.20", manifest, encoded); err != nil {
		t.Fatal(err)
	}
	if err := Verify(renamed, "v0.6.20", manifest, encoded); err == nil {
		t.Fatal("historical certificate incorrectly trusted the renamed signer/source")
	}
	if err := Verify(legacy, "v0.6.21", manifest, encoded); err == nil {
		t.Fatal("historical certificate incorrectly trusted another tag")
	}
	changed := append([]byte(nil), manifest...)
	changed[0] ^= 1
	if err := Verify(repo, "v0.6.20", changed, encoded); err == nil {
		t.Fatal("historical signature accepted a changed manifest")
	}
	var data map[string]any
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	data["dsseEnvelope"].(map[string]any)["signatures"].([]any)[0].(map[string]any)["sig"] = "AA=="
	changedBundle, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(repo, "v0.6.20", manifest, changedBundle); err == nil {
		t.Fatal("historical identity selection accepted a changed signature")
	}
}

func candidateBundle(t *testing.T, identities ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	for _, identity := range identities {
		uri, err := url.Parse(identity)
		if err != nil {
			t.Fatal(err)
		}
		certificate.URIs = append(certificate.URIs, uri)
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"certificate":{"rawBytes":"` + base64.StdEncoding.EncodeToString(der) + `"}},"dsseEnvelope":{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[{"sig":"AA=="}]}}`)
}

func TestCandidateRepositorySelectsOnlyExactCertificateWorkflowAndTag(t *testing.T) {
	for _, repo := range []string{"owner/old", "owner/new"} {
		t.Run(repo, func(t *testing.T) {
			encoded := candidateBundle(t, "https://github.com/"+repo+"/.github/workflows/release.yml@refs/tags/v1.2.3")
			candidate, err := CandidateRepository("v1.2.3", encoded, "owner/old", "owner/new")
			if err != nil || candidate != repo {
				t.Fatal(candidate, err)
			}
			// Selection is not authentication: even a perfectly matching SAN in
			// an untrusted certificate must still fail the production verifier.
			if err := Verify(candidate, "v1.2.3", []byte("manifest"), encoded); err == nil {
				t.Fatal("candidate selection admitted an untrusted certificate or invalid signature")
			}
		})
	}
	for _, identities := range [][]string{
		{"https://github.com/attacker/project/.github/workflows/release.yml@refs/tags/v1.2.3"},
		{"https://github.com/owner/old/.github/workflows/other.yml@refs/tags/v1.2.3"},
		{"https://github.com/owner/old/.github/workflows/release.yml@refs/tags/v2.0.0"},
		{"https://github.com/owner/old/.github/workflows/release.yml@refs/heads/main"},
		{"https://github.com/owner/old/.github/workflows/release.yml@refs/tags/v1.2.3", "https://github.com/owner/new/.github/workflows/release.yml@refs/tags/v1.2.3"},
		nil,
	} {
		if repo, err := CandidateRepository("v1.2.3", candidateBundle(t, identities...), "owner/old", "owner/new"); err == nil {
			t.Fatalf("accepted unexpected certificate identities %v as %s", identities, repo)
		}
	}
	for _, encoded := range [][]byte{nil, []byte("{}"), []byte("garbage"), []byte(`{"verificationMaterial":{"certificate":{"rawBytes":"invalid"}}}`), make([]byte, (4<<20)+1)} {
		if _, err := CandidateRepository("v1.2.3", encoded, "owner/old", "owner/new"); err == nil {
			t.Fatal("accepted malformed provenance")
		}
	}
}
