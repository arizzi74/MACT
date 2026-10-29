package distribution

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type bootstrapFixture struct {
	root, bin, manager, checksum, provenance, script string
	environment                                      []string
	key                                              ed25519.PrivateKey
}

func bootstrapTestFixture(t *testing.T) bootstrapFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bootstrap")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"uname", "mktemp", "chmod", "rm", "grep", "wc", "mv", "sha256sum"} {
		source, err := exec.LookPath(name)
		if err != nil {
			if name == "sha256sum" {
				name = "shasum"
				source, err = exec.LookPath(name)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(source, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	// There is deliberately no Python (or Go) anywhere in this PATH.
	if err := os.Remove(filepath.Join(bin, "uname")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bin, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -s) printf '%s\\n' \"${TEST_OS:-Linux}\" ;; -m) printf '%s\\n' \"${TEST_ARCH:-x86_64}\" ;; esac\n"))
	if err := os.Chmod(filepath.Join(bin, "uname"), 0755); err != nil {
		t.Fatal(err)
	}
	manager := []byte("#!/bin/sh\nprintf '%s\\n' \"$0\" \"$CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE\" \"$@\" > \"$TEST_RECORD\"\nexit 17\n")
	managerPath := filepath.Join(root, "manager")
	writeTestFile(t, managerPath, manager)
	manifestPath := filepath.Join(root, "manifest")
	provenancePath := filepath.Join(root, "provenance")
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexec \"$TEST_VERIFIER\" -test.run=^TestBootstrapTrustedVerifierProcess$ -- \"$@\"\n"))
	if err := os.Chmod(filepath.Join(bin, "gh"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, manifestPath, manifest(map[string][]byte{"codex-telegramgw-linux-amd64": manager}))
	// Fake curl checks transport flags, records exact URLs and returns fixture bytes.
	curl := `#!/bin/sh
set -eu
output= writeout= proto= redirect= tls=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --write-out) writeout=$2; shift 2 ;;
    --proto) proto=$2; shift 2 ;;
    --proto-redir) redirect=$2; shift 2 ;;
    --tlsv1.2) tls=yes; shift ;;
    --connect-timeout|--max-time|--retry|--max-filesize) shift 2 ;;
    --fail|--silent|--show-error|--location) shift ;;
    https://*) url=$1; shift ;;
    *) exit 99 ;;
  esac
done
[ "$proto" = '=https' ] && [ "$redirect" = '=https' ] && [ "$tls" = yes ] || exit 98
printf '%s\n' "$url" >> "$TEST_REQUESTS"
if [ "${TEST_INTERRUPT:-}" = "${url##*/}" ]; then
  printf partial > "$output"
  exit 28
fi
case "$url" in
  */releases/latest) printf '%s' "$TEST_RELEASE_URL" ;;
  */releases/tag/*) printf '%s' "${TEST_PINNED_RELEASE_URL:-$url}" ;;
  */SHA256SUMS.sigstore.json) /bin/cp "$TEST_PROVENANCE" "$output" ;;
  */SHA256SUMS) /bin/cp "$TEST_MANIFEST" "$output" ;;
  */codex-telegramgw-*) /bin/cp "$TEST_MANAGER" "$output" ;;
  *) exit 97 ;;
esac
`
	writeTestFile(t, filepath.Join(bin, "curl"), []byte(curl))
	if err := os.Chmod(filepath.Join(bin, "curl"), 0755); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	fixture := bootstrapFixture{root: root, bin: bin, manager: managerPath, checksum: manifestPath, provenance: provenancePath, script: script, key: private, environment: []string{
		"PATH=" + bin, "TMPDIR=" + root, "TEST_RECORD=" + filepath.Join(root, "record"), "TEST_REQUESTS=" + filepath.Join(root, "requests"),
		"TEST_MANIFEST=" + manifestPath, "TEST_MANAGER=" + managerPath,
		"TEST_PROVENANCE=" + provenancePath, "TEST_VERIFIER=" + verifier, "TEST_TRUSTED_VERIFIER=1", "TEST_VERIFIER_PUBLIC=" + base64.StdEncoding.EncodeToString(public),
		"TEST_RELEASE_URL=https://github.com/arizzi74/MACT/releases/tag/v1.2.3",
	}}
	fixture.sign(t, "arizzi74/MACT", "v1.2.3")
	return fixture
}

func (fixture bootstrapFixture) run(arguments ...string) ([]byte, error) {
	command := exec.Command("/bin/sh", append([]string{fixture.script}, arguments...)...)
	command.Env = fixture.environment
	return command.CombinedOutput()
}

func TestBootstrapWorksWithoutPythonAndPreservesArguments(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	output, err := fixture.run("install", "worker", "--config", "/tmp/a config.json", "--auto-update")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("manager status was not propagated: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "record"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if strings.Join(lines[1:], "\n") != "v1.2.3\ninstall\nworker\n--config\n/tmp/a config.json\n--auto-update" {
		t.Fatalf("arguments changed: %q", lines)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("temporary manager not removed: %v", err)
	}
	requests, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://github.com/arizzi74/MACT/releases/latest\nhttps://github.com/arizzi74/MACT/releases/download/v1.2.3/SHA256SUMS\nhttps://github.com/arizzi74/MACT/releases/download/v1.2.3/SHA256SUMS.sigstore.json\nhttps://github.com/arizzi74/MACT/releases/download/v1.2.3/codex-telegramgw-linux-amd64\n"
	if string(requests) != want {
		t.Fatalf("wrong release requests: %s", requests)
	}
}

func TestBootstrapWithoutArgumentsStartsGuidedSetup(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	// Simulate curl piping the bootstrap to sh: no filename or shell arguments.
	script, err := os.ReadFile(fixture.script)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh")
	command.Env = fixture.environment
	command.Stdin = strings.NewReader(string(script))
	output, err := command.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("manager status was not propagated: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "record"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if strings.Join(lines[1:], "\n") != "v1.2.3\nsetup" {
		t.Fatalf("unexpected default invocation: %q", lines)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) {
		t.Fatalf("temporary manager not removed: %v", err)
	}
}

func TestBootstrapPinnedVersionAndRepository(t *testing.T) {
	for _, args := range [][]string{
		{"install", "worker", "--version", "v2.3.4", "--repo", "owner/repository"},
		{"install", "worker", "--version=v2.3.4", "--repo=owner/repository"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.sign(t, "owner/repository", "v2.3.4")
			output, err := fixture.run(args...)
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("manager failed: %v %s", err, output)
			}
			requests, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			for _, request := range strings.Fields(string(requests)) {
				if request != "https://github.com/owner/repository/releases/tag/v2.3.4" && !strings.HasPrefix(request, "https://github.com/owner/repository/releases/download/v2.3.4/") {
					t.Fatalf("wrong repository or version: %s", request)
				}
			}
		})
	}
}

func TestBootstrapRejectsInvalidSelectionBeforeNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"--version", "v1.2.3-rc1"}, {"--version", "../bad"}, {"--version"}, {"--version="},
		{"--version=v01.2.3"}, {"--version=v1.2"}, {"--version=v1.2.3.4"}, {"--version=v1.2.3", "--version=v2.0.0"},
		{"--repo"}, {"--repo="}, {"--repo=owner/../repository"}, {"--repo=../repository"}, {"--repo=owner/repository", "--repo=another/repository"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			if output, err := fixture.run(args...); err == nil {
				t.Fatalf("accepted invalid options: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "requests")); !os.IsNotExist(err) {
				t.Fatal("invalid options caused a download")
			}
		})
	}
}

func TestBootstrapRejectsUnexpectedLatestRedirect(t *testing.T) {
	for _, url := range []string{
		"https://github.com/arizzi74/MACT/releases/tag/v1.2.3-rc1",
		"http://github.com/arizzi74/MACT/releases/tag/v1.2.3",
		"https://github.com/attacker/repo/releases/tag/v1.2.3",
		"https://github.com/arizzi74/MACT/releases/tag/extra/v1.2.3",
		"https://github.com/arizzi74/Codex-Telegram-Gateway/releases/tag/v1.2.3",
	} {
		t.Run(url, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.environment[len(fixture.environment)-1] = "TEST_RELEASE_URL=" + url
			if output, err := fixture.run("install", "worker"); err == nil {
				t.Fatalf("accepted unexpected redirect: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
				t.Fatal("executed manager after unexpected redirect")
			}
		})
	}
}

func TestBootstrapFollowsOnlyOfficialRenameForLatestAndPinnedReleases(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			resolved := "https://github.com/arizzi74/MACT/releases/tag/v1.2.3"
			fixture.environment[len(fixture.environment)-1] = "TEST_RELEASE_URL=" + resolved
			arguments := []string{"install", "worker", "--repo", "arizzi74/Codex-Telegram-Gateway"}
			if pinned {
				fixture.environment = append(fixture.environment, "TEST_PINNED_RELEASE_URL="+resolved)
				arguments = append(arguments, "--version", "v1.2.3")
			}
			output, err := fixture.run(arguments...)
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("renamed repository bootstrap failed: %v %s", err, output)
			}
			data, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			requests := strings.Fields(string(data))
			if len(requests) != 4 {
				t.Fatalf("unexpected requests: %s", data)
			}
			if !strings.HasPrefix(requests[0], "https://github.com/arizzi74/Codex-Telegram-Gateway/releases/") {
				t.Fatalf("legacy selection did not resolve through its original URL: %s", requests[0])
			}
			for _, request := range requests[1:] {
				if !strings.HasPrefix(request, "https://github.com/arizzi74/MACT/releases/download/v1.2.3/") {
					t.Fatalf("asset did not use canonical renamed repository: %s", request)
				}
			}
		})
	}
}

func TestBootstrapRenameDoesNotChangeForkSelectionOrPinnedTag(t *testing.T) {
	for _, tc := range []struct{ name, repo, resolved string }{
		{"fork remains independent", "owner/fork", "https://github.com/arizzi74/MACT/releases/tag/v1.2.3"},
		{"rename cannot change pinned tag", "arizzi74/Codex-Telegram-Gateway", "https://github.com/arizzi74/MACT/releases/tag/v2.0.0"},
		{"rename cannot change owner", "arizzi74/Codex-Telegram-Gateway", "https://github.com/attacker/MACT/releases/tag/v1.2.3"},
		{"rename cannot add URL suffix", "arizzi74/Codex-Telegram-Gateway", "https://github.com/arizzi74/MACT/releases/tag/v1.2.3?other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.environment = append(fixture.environment, "TEST_PINNED_RELEASE_URL="+tc.resolved)
			if output, err := fixture.run("install", "worker", "--repo", tc.repo, "--version", "v1.2.3"); err == nil {
				t.Fatalf("accepted unexpected canonical repository or tag: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
				t.Fatal("executed manager after invalid release resolution")
			}
		})
	}
}

func TestBootstrapRejectsPrebridgeManagersOnlyAfterOfficialRename(t *testing.T) {
	for _, tag := range []string{"v0.0.0", "v0.5.99", "v0.6.0", "v0.6.9", "v0.6.19", "v0.6.20", "v0.6.21", "v0.6.100", "v0.7.0", "v1.0.0"} {
		t.Run(tag, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.environment = append(fixture.environment, "TEST_PINNED_RELEASE_URL=https://github.com/arizzi74/MACT/releases/tag/"+tag)
			fixture.sign(t, "arizzi74/MACT", tag)
			output, err := fixture.run("install", "worker", "--version", tag)
			allowed := tag == "v0.6.21" || tag == "v0.6.100" || tag == "v0.7.0" || tag == "v1.0.0"
			if allowed {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
					t.Fatalf("compatible manager rejected: %v %s", err, output)
				}
			} else {
				if err == nil || !strings.Contains(string(output), "install v0.6.21 or newer") {
					t.Fatalf("missing prebridge explanation: %v %s", err, output)
				}
				if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
					t.Fatal("executed prebridge manager after repository rename")
				}
				data, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
				if err != nil || len(strings.Fields(string(data))) != 1 {
					t.Fatalf("downloaded manager before prebridge rejection: %s %v", data, err)
				}
			}
		})
	}
	// Before the actual rename the old historical installer remains usable.
	fixture := bootstrapTestFixture(t)
	fixture.sign(t, "arizzi74/Codex-Telegram-Gateway", "v0.6.20")
	output, err := fixture.run("install", "worker", "--repo", "arizzi74/Codex-Telegram-Gateway", "--version", "v0.6.20")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("unrenamed legacy repository rejected: %v %s", err, output)
	}
	fixture = bootstrapTestFixture(t)
	output, err = fixture.run("install", "worker", "--repo", "arizzi74/MACT", "--version", "v0.6.20")
	if err == nil || !strings.Contains(string(output), "install v0.6.21 or newer") {
		t.Fatalf("explicit canonical repository executed prebridge manager: %v %s", err, output)
	}
	fixture = bootstrapTestFixture(t)
	fixture.sign(t, "owner/fork", "v0.6.20")
	output, err = fixture.run("install", "worker", "--repo", "owner/fork", "--version", "v0.6.20")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("official prebridge guard affected a fork: %v %s", err, output)
	}
}

func TestBootstrapRejectsCorruptionAndMalformedManifests(t *testing.T) {
	for _, scenario := range []string{"corrupt manager", "duplicate", "alias duplicate", "unsafe name", "missing", "bad hash", "malformed", "empty"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			data, err := os.ReadFile(fixture.checksum)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "corrupt manager":
				writeTestFile(t, fixture.manager, []byte("malicious code"))
			case "duplicate":
				data = append(data, data...)
			case "alias duplicate":
				data = append(data, []byte(strings.Replace(string(data), "  codex-", "  ./codex-", 1))...)
			case "unsafe name":
				data = []byte(strings.Replace(string(data), "  codex-", "  ../codex-", 1))
			case "missing":
				data = []byte(strings.Replace(string(data), "codex-telegramgw-linux-amd64", "other", 1))
			case "bad hash":
				data[0] = 'z'
			case "malformed":
				data = []byte("invalid\n")
			case "empty":
				data = nil
			}
			writeTestFile(t, fixture.checksum, data)
			fixture.sign(t, "arizzi74/MACT", "v1.2.3")
			if output, err := fixture.run("install", "worker"); err == nil {
				t.Fatalf("accepted tampered release: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
				t.Fatal("executed unverified manager")
			}
		})
	}
}

func TestBootstrapSelectsEverySupportedPlatform(t *testing.T) {
	for _, target := range Targets() {
		t.Run(target.Suffix(), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			osName, arch := "Linux", "x86_64"
			if target.OS == "darwin" {
				osName = "Darwin"
			}
			if target.Arch == "arm64" {
				arch = "arm64"
			}
			fixture.environment = append(fixture.environment, "TEST_OS="+osName, "TEST_ARCH="+arch)
			data, err := os.ReadFile(fixture.manager)
			if err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, fixture.checksum, []byte(fmt.Sprintf("%s  %s\n", digest(data), target.ManagerAsset())))
			fixture.sign(t, "arizzi74/MACT", "v1.2.3")
			output, err := fixture.run("install", "worker")
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("platform selection failed: %v %s", err, output)
			}
		})
	}
}

// This verifier is installed by the trusted test harness before the bootstrap
// starts. It cryptographically authenticates synthetic, local release fixtures
// with a separately held public key, while enforcing the gh invocation contract.
// It does not claim to replace integration testing of GitHub CLI's Sigstore code.
type bootstrapClaim struct {
	Repository, Identity, Issuer, Ref, Predicate, Digest string
}

type bootstrapEnvelope struct {
	Claim     json.RawMessage
	Signature []byte
}

func (fixture bootstrapFixture) sign(t *testing.T, repository, tag string) {
	t.Helper()
	manifest, err := os.ReadFile(fixture.checksum)
	if err != nil {
		t.Fatal(err)
	}
	fixture.signClaim(t, bootstrapClaim{repository,
		"https://github.com/" + repository + "/.github/workflows/release.yml@refs/tags/" + tag,
		"https://token.actions.githubusercontent.com", "refs/tags/" + tag,
		"https://slsa.dev/provenance/v1", digest(manifest)})
}

func (fixture bootstrapFixture) signClaim(t *testing.T, claim bootstrapClaim) {
	t.Helper()
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(bootstrapEnvelope{data, ed25519.Sign(fixture.key, data)})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, fixture.provenance, encoded)
}

func TestBootstrapTrustedVerifierProcess(t *testing.T) {
	if os.Getenv("TEST_TRUSTED_VERIFIER") != "1" {
		return
	}
	separator := 0
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i + 1
			break
		}
	}
	args := os.Args[separator:]
	if len(args) == 3 && strings.Join(args, " ") == "attestation verify --help" {
		return
	}
	if len(args) != 15 || args[0] != "attestation" || args[1] != "verify" {
		t.Fatal("incorrect verifier invocation")
	}
	flags := map[string]string{}
	for i := 3; i < len(args); i += 2 {
		if flags[args[i]] != "" {
			t.Fatal("duplicate verifier flag")
		}
		flags[args[i]] = args[i+1]
	}
	data, err := os.ReadFile(flags["--bundle"])
	if err != nil {
		t.Fatal("missing provenance")
	}
	var envelope bootstrapEnvelope
	if json.Unmarshal(data, &envelope) != nil {
		t.Fatal("malformed provenance")
	}
	public, err := base64.StdEncoding.DecodeString(os.Getenv("TEST_VERIFIER_PUBLIC"))
	if err != nil || len(public) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(public), envelope.Claim, envelope.Signature) {
		t.Fatal("invalid signature")
	}
	var claim bootstrapClaim
	if json.Unmarshal(envelope.Claim, &claim) != nil {
		t.Fatal("malformed claim")
	}
	if flags["--repo"] != claim.Repository || flags["--cert-identity"] != claim.Identity || flags["--cert-oidc-issuer"] != claim.Issuer || flags["--source-ref"] != claim.Ref || flags["--predicate-type"] != claim.Predicate {
		t.Fatal("wrong publisher identity")
	}
	if len(flags) != 6 {
		t.Fatal("missing or extra verifier flags")
	}
	manifest, err := os.ReadFile(args[2])
	if err != nil || digest(manifest) != claim.Digest {
		t.Fatal("unauthenticated manifest digest")
	}
}

func TestBootstrapRejectsUnauthenticatedManagerBeforeExecution(t *testing.T) {
	for _, component := range []string{"worker", "gateway"} {
		for _, scenario := range []string{"matching malicious checksums", "wrong repository", "wrong workflow", "wrong tag", "wrong source ref", "wrong issuer", "wrong predicate", "bad signature", "missing provenance", "malformed provenance", "interrupted manifest", "interrupted provenance", "interrupted manager", "missing verifier", "old verifier"} {
			t.Run(component+"/"+scenario, func(t *testing.T) {
				fixture := bootstrapTestFixture(t)
				data, err := os.ReadFile(fixture.checksum)
				if err != nil {
					t.Fatal(err)
				}
				claim := bootstrapClaim{"arizzi74/MACT", "https://github.com/arizzi74/MACT/.github/workflows/release.yml@refs/tags/v1.2.3", "https://token.actions.githubusercontent.com", "refs/tags/v1.2.3", "https://slsa.dev/provenance/v1", digest(data)}
				switch scenario {
				case "matching malicious checksums":
					malicious := []byte("#!/bin/sh\nprintf compromised > \"$TEST_RECORD\"\n")
					writeTestFile(t, fixture.manager, malicious)
					writeTestFile(t, fixture.checksum, manifest(map[string][]byte{"codex-telegramgw-linux-amd64": malicious}))
				case "wrong repository":
					claim.Repository = "attacker/project"
				case "wrong workflow":
					claim.Identity = strings.ReplaceAll(claim.Identity, "release.yml", "evil.yml")
				case "wrong tag":
					claim.Identity = strings.ReplaceAll(claim.Identity, "v1.2.3", "v1.2.4")
				case "wrong source ref":
					claim.Ref = "refs/heads/main"
				case "wrong issuer":
					claim.Issuer = "https://issuer.invalid"
				case "wrong predicate":
					claim.Predicate = "https://predicate.invalid"
				case "bad signature":
					encoded, _ := json.Marshal(bootstrapEnvelope{json.RawMessage(`{}`), make([]byte, ed25519.SignatureSize)})
					writeTestFile(t, fixture.provenance, encoded)
				case "missing provenance":
					if err := os.Remove(fixture.provenance); err != nil {
						t.Fatal(err)
					}
				case "malformed provenance":
					writeTestFile(t, fixture.provenance, []byte("invalid JSON"))
				case "interrupted manifest":
					fixture.environment = append(fixture.environment, "TEST_INTERRUPT=SHA256SUMS")
				case "interrupted provenance":
					fixture.environment = append(fixture.environment, "TEST_INTERRUPT=SHA256SUMS.sigstore.json")
				case "interrupted manager":
					fixture.environment = append(fixture.environment, "TEST_INTERRUPT=codex-telegramgw-linux-amd64")
				case "missing verifier":
					if err := os.Remove(filepath.Join(fixture.bin, "gh")); err != nil {
						t.Fatal(err)
					}
				case "old verifier":
					writeTestFile(t, filepath.Join(fixture.bin, "gh"), []byte("#!/bin/sh\nexit 1\n"))
				}
				if strings.HasPrefix(scenario, "wrong ") {
					fixture.signClaim(t, claim)
				}
				if output, err := fixture.run("setup", component); err == nil {
					t.Fatalf("accepted unauthenticated installer: %s", output)
				}
				if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
					t.Fatal("executed unauthenticated code")
				}
				remaining, err := filepath.Glob(filepath.Join(fixture.root, "codex-telegramgw-install.*"))
				if err != nil || len(remaining) != 0 {
					t.Fatalf("left partial installation: %v %v", remaining, err)
				}
			})
		}
	}
}

func TestDocumentedInstallerAuthenticatesBeforeExecution(t *testing.T) {
	guide, err := os.ReadFile("../../docs/installation.md")
	if err != nil {
		t.Fatal(err)
	}
	section := strings.SplitN(string(guide), "## Download and authenticate the installer", 2)
	if len(section) != 2 {
		t.Fatal("missing authenticated installation guide")
	}
	block := strings.SplitN(section[1], "```sh\n", 2)
	if len(block) != 2 {
		t.Fatal("missing installation command")
	}
	command := strings.SplitN(block[1], "```", 2)[0]
	if strings.Count(command, "version=v0.7.8") != 1 {
		t.Fatal("missing explicit stable installer version")
	}
	command = strings.Replace(command, "version=v0.7.8", "version=v1.2.3", 1)
	for _, scenario := range []string{"authenticated", "matching malicious checksums", "substituted manager", "wrong tag", "missing provenance", "interrupted download"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			switch scenario {
			case "matching malicious checksums", "substituted manager":
				malicious := []byte("#!/bin/sh\nprintf compromised > \"$TEST_RECORD\"\n")
				writeTestFile(t, fixture.manager, malicious)
				if scenario == "matching malicious checksums" {
					writeTestFile(t, fixture.checksum, manifest(map[string][]byte{"codex-telegramgw-linux-amd64": malicious}))
				}
			case "wrong tag":
				fixture.sign(t, "arizzi74/MACT", "v1.2.4")
			case "missing provenance":
				if err := os.Remove(fixture.provenance); err != nil {
					t.Fatal(err)
				}
			case "interrupted download":
				fixture.environment = append(fixture.environment, "TEST_INTERRUPT=codex-telegramgw-linux-amd64")
			}
			// The documented assignment is tested literally. Its failure leaves no
			// callable install path even if a terminal user continues to the next command.
			cmd := exec.Command("/bin/sh", "-c", command+"\n[ -n \"$MACT_INSTALLER_DIR\" ] || exit 1\n\"$MACT_INSTALLER_DIR/install\" setup worker")
			cmd.Env = fixture.environment
			output, err := cmd.CombinedOutput()
			if scenario == "authenticated" {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
					t.Fatalf("authenticated install failed: %v %s", err, output)
				}
				if _, err := os.Stat(filepath.Join(fixture.root, "record")); err != nil {
					t.Fatal("authenticated fixture was not executed")
				}
			} else {
				if err == nil {
					t.Fatalf("accepted unverified installer: %s", output)
				}
				if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
					t.Fatal("documented instructions executed unverified code")
				}
				remaining, err := filepath.Glob(filepath.Join(fixture.root, "mact-installer.*"))
				if err != nil || len(remaining) != 0 {
					t.Fatalf("left partial download: %v %v", remaining, err)
				}
			}
		})
	}
}

func TestBootstrapPrivilegedGatewayRejectsSubstitutedManager(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux root gateway installation")
	}
	fixture := bootstrapTestFixture(t)
	malicious := []byte("#!/bin/sh\nprintf compromised > \"$TEST_RECORD\"\n")
	writeTestFile(t, fixture.manager, malicious)
	writeTestFile(t, fixture.checksum, manifest(map[string][]byte{"codex-telegramgw-linux-amd64": malicious}))
	var command *exec.Cmd
	if os.Geteuid() == 0 {
		command = exec.Command("/bin/sh", fixture.script, "setup", "gateway")
		command.Env = fixture.environment
	} else {
		sudo, err := exec.LookPath("sudo")
		if err != nil || exec.Command(sudo, "-n", "true").Run() != nil {
			t.Skip("requires root or passwordless sudo; no real installation is performed")
		}
		args := append([]string{"-n", "env"}, fixture.environment...)
		args = append(args, "/bin/sh", fixture.script, "setup", "gateway")
		command = exec.Command(sudo, args...)
	}
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "release provenance verification failed") {
		t.Fatalf("privileged installer did not reject provenance: %v %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
		t.Fatal("executed unauthenticated code as root")
	}
}

func TestBootstrapUsesPortableShasumFallback(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	shasum, err := exec.LookPath("shasum")
	if err != nil {
		t.Skip("shasum is not installed on this test host")
	}
	if err := os.Remove(filepath.Join(fixture.bin, "sha256sum")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.bin, "shasum")); os.IsNotExist(err) {
		if err := os.Symlink(shasum, filepath.Join(fixture.bin, "shasum")); err != nil {
			t.Fatal(err)
		}
	}
	if output, err := fixture.run("setup", "worker"); err == nil {
		t.Fatalf("fixture did not propagate manager status: %s", output)
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("shasum fallback failed: %v %s", err, output)
	}
}

func TestBootstrapRejectsVerifierWithoutRequiredPolicyFlags(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	// Some old verifiers have the command but lack exact source-ref policy.
	writeTestFile(t, filepath.Join(fixture.bin, "gh"), []byte("#!/bin/sh\n[ \"$3\" = --help ] && exit 0\nexit 1\n"))
	if output, err := fixture.run("setup", "gateway"); err == nil {
		t.Fatalf("accepted unsupported verification policy: %s", output)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
		t.Fatal("executed manager without complete publisher verification")
	}
}
