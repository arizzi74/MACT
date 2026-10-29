package distribution

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type bootstrapFixture struct {
	root, bin, home, manager, checksum, provenance, script string
	environment                                            []string
	key                                                    ed25519.PrivateKey
}

// Independently checked against the published v0.7.10 SHA256SUMS manifest, whose
// SHA256 is 5c24e4581f704bceb1f4c54e1a0efb9af11badc72714d10a15b676138071b8f3.
var bootstrapPins = map[string]string{
	"linux-amd64":  "14679733a214051917367b297c2b1053f8a9aff2aca3ae1ac15382b967887c96",
	"linux-arm64":  "50644f55ac39030140e2de84387f71ba397f7c8285037e3e33d6756a00a958cb",
	"darwin-amd64": "0ef44ca5d470166944ef71d17657a51800a3d7defeaf0351012c24110992abf1",
	"darwin-arm64": "af5085990b286575c98ac839112a2b6d6c232d8b8cd097812bd5eef52dc4d7cc",
}

func bootstrapTestFixture(t *testing.T) bootstrapFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX bootstrap")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	home := filepath.Join(root, "home")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0700); err != nil {
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
	// There is deliberately no gh, Python, or Go anywhere in this PATH.
	if err := os.Remove(filepath.Join(bin, "uname")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bin, "uname"), []byte("#!/bin/sh\ncase \"$1\" in -s) printf '%s\\n' \"${TEST_OS:-Linux}\" ;; -m) printf '%s\\n' \"${TEST_ARCH:-x86_64}\" ;; esac\n"))
	if err := os.Chmod(filepath.Join(bin, "uname"), 0755); err != nil {
		t.Fatal(err)
	}
	// This manager records the shell's forwarding contract. It does not emulate
	// the native CLI or execute installation; use supported invocation shapes.
	manager := []byte(`#!/bin/sh
if [ "$#" -eq 1 ] && [ "$1" = version ] && [ -z "${CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE:-}" ]; then
  printf '%s\n' "$0" >> "$TEST_PROBES"
  if [ -n "${HOME:-}" ]; then
    case "$0" in "$HOME"/*) exit "${TEST_HOME_PROBE_STATUS:-0}" ;; esac
  fi
  exit "${TEST_PROBE_STATUS:-0}"
fi
printf '%s\n' "$0" >> "$TEST_INVOCATIONS"
printf '%s\n' "$TMPDIR" > "$TEST_TMPDIR_RECORD"
printf '%s\n' "$0" "$CODEX_TELEGRAMGW_BOOTSTRAP_RELEASE" "$@" > "$TEST_RECORD"
exit "${TEST_MANAGER_STATUS:-17}"
`)
	managerPath := filepath.Join(root, "manager")
	writeTestFile(t, managerPath, manager)
	manifestPath := filepath.Join(root, "manifest")
	provenancePath := filepath.Join(root, "provenance")
	chmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bin, "chmod")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(bin, "chmod"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$TEST_CHMODS\"\nexec \"$TEST_REAL_CHMOD\" \"$@\"\n"))
	if err := os.Chmod(filepath.Join(bin, "chmod"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, manifestPath, manifest(map[string][]byte{"codex-telegramgw-linux-amd64": manager}))
	// Fake curl checks transport flags, records exact URLs and returns fixture bytes.
	curl := `#!/bin/sh
set -eu
output= writeout= proto= redirect= tls= timeout= connect= retry= limit= fail= silent= errors= location=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) output=$2; shift 2 ;;
    --write-out) writeout=$2; shift 2 ;;
    --proto) proto=$2; shift 2 ;;
    --proto-redir) redirect=$2; shift 2 ;;
    --tlsv1.2) tls=yes; shift ;;
    --connect-timeout) connect=$2; shift 2 ;;
    --max-time) timeout=$2; shift 2 ;;
    --retry) retry=$2; shift 2 ;;
    --max-filesize) limit=$2; shift 2 ;;
    --fail) fail=yes; shift ;;
    --silent) silent=yes; shift ;;
    --show-error) errors=yes; shift ;;
    --location) location=yes; shift ;;
    https://*) url=$1; shift ;;
    *) exit 99 ;;
  esac
done
[ "$proto" = '=https' ] && [ "$redirect" = '=https' ] && [ "$tls" = yes ] || exit 98
[ "$connect" = 15 ] && [ "$timeout" = 180 ] && [ "$retry" = 2 ] || exit 96
[ "$fail$silent$errors$location" = yesyesyesyes ] || exit 95
case "$url" in
  */codex-telegramgw-*) [ "$limit" = 67108864 ] ;;
  */SHA256SUMS.sigstore.json) [ "$limit" = 4194304 ] ;;
  *) [ "$limit" = 1048576 ] ;;
esac || exit 94
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
	script, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Substitute only the reviewed pins in a private script copy, so synthetic
	// manager bytes exercise actual checksum verification without network access
	// or a production runtime override for the trust root. Production pins and
	// rejection of these fixture bytes are checked separately below.
	for target, pin := range bootstrapPins {
		needle := target + ") expected=" + pin + " ;;"
		if strings.Count(string(script), needle) != 1 {
			t.Fatalf("missing or duplicate production pin for %s", target)
		}
		script = []byte(strings.Replace(string(script), needle, target+") expected="+digest(manager)+" ;;", 1))
	}
	scriptPath := filepath.Join(root, "install.sh")
	writeTestFile(t, scriptPath, script)
	fixture := bootstrapFixture{root: root, bin: bin, home: home, manager: managerPath, checksum: manifestPath, provenance: provenancePath, script: scriptPath, environment: []string{
		"PATH=" + bin, "TMPDIR=" + root, "HOME=" + home, "TEST_RECORD=" + filepath.Join(root, "record"), "TEST_REQUESTS=" + filepath.Join(root, "requests"),
		"TEST_PROBES=" + filepath.Join(root, "probes"), "TEST_INVOCATIONS=" + filepath.Join(root, "invocations"), "TEST_TMPDIR_RECORD=" + filepath.Join(root, "manager-tmpdir"),
		"TEST_MANIFEST=" + manifestPath, "TEST_MANAGER=" + managerPath,
		"TEST_PROVENANCE=" + provenancePath, "TEST_REAL_CHMOD=" + chmod, "TEST_CHMODS=" + filepath.Join(root, "chmods"),
		"TEST_RELEASE_URL=https://github.com/arizzi74/MACT/releases/tag/v1.2.3",
	}}
	return fixture
}

func (fixture bootstrapFixture) run(arguments ...string) ([]byte, error) {
	command := exec.Command("/bin/sh", append([]string{fixture.script}, arguments...)...)
	command.Env = fixture.environment
	return command.CombinedOutput()
}

func TestBootstrapProductionPinsRejectSubstitutedManagersOnEveryPlatform(t *testing.T) {
	script, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "bootstrap_version=v0.7.10\n") || len(bootstrapPins) != len(Targets()) {
		t.Fatal("bootstrap version or supported target pins changed")
	}
	for _, target := range Targets() {
		t.Run(target.Suffix(), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			// Run the unmodified production pins against substituted fixture bytes.
			writeTestFile(t, fixture.script, script)
			osName := "Linux"
			if target.OS == "darwin" {
				osName = "Darwin"
			}
			fixture.environment = append(fixture.environment, "TEST_OS="+osName, "TEST_ARCH="+target.Arch)
			output, err := fixture.run("setup", "worker")
			if err == nil || !strings.Contains(string(output), "installer checksum verification failed") {
				t.Fatalf("production pin did not reject substituted bytes: %v %s", err, output)
			}
			for _, name := range []string{"record", "chmods", "probes", "invocations"} {
				if _, err := os.Stat(filepath.Join(fixture.root, name)); !os.IsNotExist(err) {
					t.Fatalf("used substituted manager (%s): %v", name, err)
				}
			}
		})
	}
}

func TestBootstrapWorksWithoutGhPythonOrGoAndPreservesArguments(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	for _, name := range []string{"gh", "python", "python3", "go"} {
		if _, err := os.Stat(filepath.Join(fixture.bin, name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected %s dependency in test PATH: %v", name, err)
		}
	}
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
	want := "https://github.com/arizzi74/MACT/releases/latest\nhttps://github.com/arizzi74/MACT/releases/download/v0.7.10/codex-telegramgw-linux-amd64\n"
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

func TestBootstrapPreservesLiteralShellArgumentsAndCleansTemporaryDirectory(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	marker := filepath.Join(fixture.root, "injected")
	arguments := []string{"install", "worker", "--config", "$(printf injected > " + marker + ") `printf injected > " + marker + "` a; b & 'c' \"d\" *"}
	temporary := filepath.Join(fixture.root, "temporary with spaces")
	if err := os.Mkdir(temporary, 0700); err != nil {
		t.Fatal(err)
	}
	fixture.environment = append(fixture.environment, "TMPDIR="+temporary)
	output, err := fixture.run(arguments...)
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("manager failed: %v %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(fixture.root, "record"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\nv1.2.3\n"+strings.Join(arguments, "\n")+"\n") {
		t.Fatalf("shell arguments changed: %s", data)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell argument executed: %v", err)
	}
	remaining, err := os.ReadDir(temporary)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("temporary manager directory not removed: %v %v", remaining, err)
	}
}

func bootstrapRecordLines(t *testing.T, fixture bootstrapFixture, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.root, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func assertBootstrapTemporaryCleanup(t *testing.T, directories ...string) {
	t.Helper()
	for _, directory := range directories {
		for _, pattern := range []string{"codex-telegramgw-install.*", ".mact-install.*"} {
			remaining, err := filepath.Glob(filepath.Join(directory, pattern))
			if err != nil || len(remaining) != 0 {
				t.Fatalf("temporary manager directories not removed: %v %v", remaining, err)
			}
		}
	}
}

func TestBootstrapProbesExecutionWithoutRetryingSetup(t *testing.T) {
	for _, tc := range []struct {
		name, message                   string
		probe, homeProbe, manager, exit int
		probes, invocations             int
		fallback                        bool
	}{
		{name: "executable temporary directory", manager: 17, exit: 17, probes: 1, invocations: 1},
		{name: "execution denied in temporary directory", probe: 126, manager: 17, exit: 17, probes: 2, invocations: 1, fallback: true},
		{name: "other startup failure", probe: 42, manager: 17, exit: 1, probes: 1, message: "startup failed (exit 42)"},
		{name: "missing interpreter", probe: 127, manager: 17, exit: 1, probes: 1, message: "startup failed (exit 127)"},
		{name: "home execution also denied", probe: 126, homeProbe: 126, manager: 17, exit: 1, probes: 2, message: "TMPDIR"},
		{name: "setup returns 126", manager: 126, exit: 126, probes: 1, invocations: 1},
		{name: "relocated setup returns 126", probe: 126, manager: 126, exit: 126, probes: 2, invocations: 1, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.environment = append(fixture.environment,
				fmt.Sprintf("TEST_PROBE_STATUS=%d", tc.probe),
				fmt.Sprintf("TEST_HOME_PROBE_STATUS=%d", tc.homeProbe),
				fmt.Sprintf("TEST_MANAGER_STATUS=%d", tc.manager))
			output, err := fixture.run("setup", "worker")
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != tc.exit {
				t.Fatalf("wrong exit status: want %d, got %v %s", tc.exit, err, output)
			}
			if !strings.Contains(string(output), tc.message) {
				t.Fatalf("missing startup failure guidance %q: %s", tc.message, output)
			}
			if probes := bootstrapRecordLines(t, fixture, "probes"); len(probes) != tc.probes {
				t.Fatalf("wrong version probe count: %q", probes)
			}
			invocations := bootstrapRecordLines(t, fixture, "invocations")
			if len(invocations) != tc.invocations {
				t.Fatalf("setup must execute once after a successful probe: %q", invocations)
			}
			if tc.invocations > 0 {
				inHome := filepath.Dir(filepath.Dir(invocations[0])) == fixture.home
				if inHome != tc.fallback {
					t.Fatalf("manager executed from wrong directory: %q", invocations[0])
				}
				if tc.fallback {
					tmpdir := bootstrapRecordLines(t, fixture, "manager-tmpdir")
					if len(tmpdir) != 1 || tmpdir[0] != filepath.Dir(invocations[0]) {
						t.Fatalf("relocated manager did not receive executable TMPDIR: %q", tmpdir)
					}
				}
			}
			assertBootstrapTemporaryCleanup(t, fixture.root, fixture.home)
		})
	}
}

func TestBootstrapForwardsExplicitVersionAfterItsExecutionProbe(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	output, err := fixture.run("version")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("version status was not forwarded: %v %s", err, output)
	}
	if probes := bootstrapRecordLines(t, fixture, "probes"); len(probes) != 1 {
		t.Fatalf("wrong version probe count: %q", probes)
	}
	record := bootstrapRecordLines(t, fixture, "record")
	if len(record) != 3 || record[1] != "v1.2.3" || record[2] != "version" {
		t.Fatalf("explicit version was not forwarded: %q", record)
	}
	assertBootstrapTemporaryCleanup(t, fixture.root, fixture.home)
}

// Use an existing writable noexec mount; the test never mounts or changes one.
func bootstrapNoexecDirectory(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("requires a writable existing Linux noexec mount")
	}
	for _, candidate := range []string{"/run/lock", "/run", "/dev/shm", "/tmp"} {
		directory, err := os.MkdirTemp(candidate, "mact-bootstrap-noexec-")
		if err != nil {
			continue
		}
		probe := filepath.Join(directory, "probe")
		if err = os.WriteFile(probe, []byte("#!/bin/sh\nexit 0\n"), 0700); err == nil {
			err = exec.Command(probe).Run()
		}
		if errors.Is(err, os.ErrPermission) {
			if err := os.Remove(probe); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			return directory
		}
		_ = os.RemoveAll(directory)
	}
	t.Skip("no writable existing noexec mount found")
	return ""
}

func TestBootstrapRecoversFromRealNoexecTemporaryDirectory(t *testing.T) {
	for _, symlinkHome := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink HOME=%t", symlinkHome), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			temporary := bootstrapNoexecDirectory(t)
			home := fixture.home
			if symlinkHome {
				home = filepath.Join(fixture.root, "linked home")
				if err := os.Symlink(fixture.home, home); err != nil {
					t.Fatal(err)
				}
			}
			fixture.environment = append(fixture.environment, "TMPDIR="+temporary, "HOME="+home, "TEST_ARCH=aarch64")
			output, err := fixture.run("setup", "claude-worker")
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("noexec recovery failed: %v %s", err, output)
			}
			invocations := bootstrapRecordLines(t, fixture, "invocations")
			if len(invocations) != 1 || filepath.Dir(filepath.Dir(invocations[0])) != home {
				t.Fatalf("verified manager did not run once from HOME: %q", invocations)
			}
			if probes := bootstrapRecordLines(t, fixture, "probes"); len(probes) != 1 || probes[0] != invocations[0] {
				t.Fatalf("unexpected probes on executable HOME: %q", probes)
			}
			tmpdir := bootstrapRecordLines(t, fixture, "manager-tmpdir")
			if len(tmpdir) != 1 || tmpdir[0] != filepath.Dir(invocations[0]) {
				t.Fatalf("manager still received noexec TMPDIR: %q", tmpdir)
			}
			if record := bootstrapRecordLines(t, fixture, "record"); len(record) < 2 || strings.Join(record[1:], "\n") != "v1.2.3\nsetup\nclaude-worker" {
				t.Fatalf("relocation changed setup arguments: %q", record)
			}
			if requests := bootstrapRecordLines(t, fixture, "requests"); len(requests) != 2 || !strings.HasSuffix(requests[1], "/codex-telegramgw-linux-arm64") {
				t.Fatalf("recovery redownloaded or selected the wrong manager: %q", requests)
			}
			assertBootstrapTemporaryCleanup(t, temporary, fixture.home)
			if symlinkHome {
				if _, err := os.Readlink(home); err != nil {
					t.Fatalf("cleanup changed the HOME symlink: %v", err)
				}
			}
		})
	}
}

func TestBootstrapFailsClosedWhenTemporaryDirectoryAndHomeAreNoexec(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	temporary := bootstrapNoexecDirectory(t)
	home := filepath.Join(temporary, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	fixture.environment = append(fixture.environment, "TMPDIR="+temporary, "HOME="+home)
	output, err := fixture.run("setup", "worker")
	if err == nil || !strings.Contains(string(output), "TMPDIR") || !strings.Contains(string(output), "permits execution") {
		t.Fatalf("missing actionable noexec failure: %v %s", err, output)
	}
	for _, name := range []string{"probes", "invocations", "record"} {
		if lines := bootstrapRecordLines(t, fixture, name); len(lines) != 0 {
			t.Fatalf("executed manager from a noexec filesystem (%s): %q", name, lines)
		}
	}
	assertBootstrapTemporaryCleanup(t, temporary, home)
}

func TestBootstrapFailsClosedWithoutHomeForNoexecRecovery(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	fixture.environment = append(fixture.environment, "TEST_PROBE_STATUS=126", "HOME=")
	output, err := fixture.run("setup", "worker")
	if err == nil || !strings.Contains(string(output), "TMPDIR") || !strings.Contains(string(output), "permits execution") {
		t.Fatalf("missing actionable recovery failure: %v %s", err, output)
	}
	if invocations := bootstrapRecordLines(t, fixture, "invocations"); len(invocations) != 0 {
		t.Fatalf("setup executed without an executable location: %q", invocations)
	}
	assertBootstrapTemporaryCleanup(t, fixture.root, fixture.home)
}

func TestBootstrapRejectsManagerTamperedDuringNoexecRelocation(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	mv, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.bin, "mv")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(fixture.bin, "mv"), []byte("#!/bin/sh\n\"$TEST_REAL_MV\" \"$@\" || exit $?\nprintf corrupt > \"$3/${1##*/}\"\n"))
	if err := os.Chmod(filepath.Join(fixture.bin, "mv"), 0755); err != nil {
		t.Fatal(err)
	}
	fixture.environment = append(fixture.environment, "TEST_PROBE_STATUS=126", "TEST_REAL_MV="+mv)
	output, err := fixture.run("setup", "worker")
	if err == nil || !strings.Contains(string(output), "installer checksum verification failed") {
		t.Fatalf("accepted manager modified during relocation: %v %s", err, output)
	}
	if probes := bootstrapRecordLines(t, fixture, "probes"); len(probes) != 1 {
		t.Fatalf("probed modified relocated manager: %q", probes)
	}
	if invocations := bootstrapRecordLines(t, fixture, "invocations"); len(invocations) != 0 {
		t.Fatalf("executed setup after relocation changed manager bytes: %q", invocations)
	}
	assertBootstrapTemporaryCleanup(t, fixture.root, fixture.home)
}

func TestBootstrapSelectsTargetVersionAndRepositoryIndependentlyOfPinnedManager(t *testing.T) {
	for _, args := range [][]string{
		{"install", "worker", "--config", "/tmp/worker.json", "--version", "v2.3.4", "--repo", "owner/repository"},
		{"install", "worker", "--config", "/tmp/worker.json", "--version=v2.3.4", "--repo=owner/repository"},
		{"install", "worker", "--config", "/tmp/worker.json", "--repo=owner/repository"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.environment = append(fixture.environment, "TEST_RELEASE_URL=https://github.com/owner/repository/releases/tag/v2.3.4")
			output, err := fixture.run(args...)
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("manager failed: %v %s", err, output)
			}
			requests, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
			if err != nil {
				t.Fatal(err)
			}
			selection := "tag/v2.3.4"
			if !strings.Contains(strings.Join(args, " "), "--version") {
				selection = "latest"
			}
			want := "https://github.com/owner/repository/releases/" + selection + "\nhttps://github.com/arizzi74/MACT/releases/download/v0.7.10/codex-telegramgw-linux-amd64\n"
			if string(requests) != want {
				t.Fatalf("target selection changed bootstrap repository/version: %s", requests)
			}
			record, err := os.ReadFile(filepath.Join(fixture.root, "record"))
			if err != nil || !strings.Contains(string(record), "\nv2.3.4\n"+strings.Join(args, "\n")+"\n") {
				t.Fatalf("target release or arguments not forwarded: %s %v", record, err)
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
			arguments := []string{"install", "worker", "--config", "/tmp/worker.json", "--repo", "arizzi74/Codex-Telegram-Gateway"}
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
			if len(requests) != 2 {
				t.Fatalf("unexpected requests: %s", data)
			}
			if !strings.HasPrefix(requests[0], "https://github.com/arizzi74/Codex-Telegram-Gateway/releases/") {
				t.Fatalf("legacy selection did not resolve through its original URL: %s", requests[0])
			}
			for _, request := range requests[1:] {
				if !strings.HasPrefix(request, "https://github.com/arizzi74/MACT/releases/download/v0.7.10/") {
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

func TestBootstrapRejectsPrebridgeReleasesOnlyAfterOfficialRename(t *testing.T) {
	for _, tag := range []string{"v0.0.0", "v0.5.99", "v0.6.0", "v0.6.9", "v0.6.19", "v0.6.20", "v0.6.21", "v0.6.100", "v0.7.0", "v1.0.0"} {
		t.Run(tag, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.environment = append(fixture.environment, "TEST_PINNED_RELEASE_URL=https://github.com/arizzi74/MACT/releases/tag/"+tag)
			output, err := fixture.run("install", "worker", "--config", "/tmp/worker.json", "--version", tag)
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
	// The guard is specific to canonical target releases, including renamed URLs.
	fixture := bootstrapTestFixture(t)
	output, err := fixture.run("install", "worker", "--config", "/tmp/worker.json", "--repo", "arizzi74/Codex-Telegram-Gateway", "--version", "v0.6.20")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("unrenamed legacy repository rejected: %v %s", err, output)
	}
	fixture = bootstrapTestFixture(t)
	output, err = fixture.run("install", "worker", "--config", "/tmp/worker.json", "--repo", "arizzi74/MACT", "--version", "v0.6.20")
	if err == nil || !strings.Contains(string(output), "install v0.6.21 or newer") {
		t.Fatalf("explicit canonical repository executed prebridge manager: %v %s", err, output)
	}
	fixture = bootstrapTestFixture(t)
	output, err = fixture.run("install", "worker", "--config", "/tmp/worker.json", "--repo", "owner/fork", "--version", "v0.6.20")
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
		t.Fatalf("official prebridge guard affected a fork: %v %s", err, output)
	}
}

func TestBootstrapRejectsCorruptOrPartialManagerBeforeChmodAndExecution(t *testing.T) {
	for _, component := range []string{"worker", "gateway"} {
		for _, scenario := range []string{"corrupt", "empty", "truncated", "interrupted", "matching manifest and environment override"} {
			t.Run(component+"/"+scenario, func(t *testing.T) {
				fixture := bootstrapTestFixture(t)
				switch scenario {
				case "corrupt":
					writeTestFile(t, fixture.manager, []byte("#!/bin/sh\nprintf compromised > \"$TEST_RECORD\"\n"))
				case "matching manifest and environment override":
					malicious := []byte("#!/bin/sh\nprintf compromised > \"$TEST_RECORD\"\n")
					writeTestFile(t, fixture.manager, malicious)
					writeTestFile(t, fixture.checksum, manifest(map[string][]byte{"codex-telegramgw-linux-amd64": malicious}))
					fixture.environment = append(fixture.environment, "expected="+digest(malicious), "CODEX_TELEGRAMGW_BOOTSTRAP_SHA256="+digest(malicious))
				case "empty":
					writeTestFile(t, fixture.manager, nil)
				case "truncated":
					writeTestFile(t, fixture.manager, []byte("#!/bin/sh\n"))
				case "interrupted":
					fixture.environment = append(fixture.environment, "TEST_INTERRUPT=codex-telegramgw-linux-amd64")
				}
				output, err := fixture.run("setup", component)
				if err == nil {
					t.Fatalf("accepted corrupt manager: %s", output)
				}
				message := "installer checksum verification failed; no installer code was executed"
				if scenario == "interrupted" {
					message = "could not download the release manager"
				}
				if !strings.Contains(string(output), message) {
					t.Fatalf("wrong failure: %v %s", err, output)
				}
				for _, name := range []string{"record", "chmods", "probes", "invocations"} {
					if _, err := os.Stat(filepath.Join(fixture.root, name)); !os.IsNotExist(err) {
						t.Fatalf("used corrupt manager before rejection (%s): %v", name, err)
					}
				}
				remaining, err := filepath.Glob(filepath.Join(fixture.root, "codex-telegramgw-install.*"))
				if err != nil || len(remaining) != 0 {
					t.Fatalf("left partial installation: %v %v", remaining, err)
				}
			})
		}
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
			output, err := fixture.run("setup", "worker")
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 17 {
				t.Fatalf("platform selection failed: %v %s", err, output)
			}
			requests, err := os.ReadFile(filepath.Join(fixture.root, "requests"))
			if err != nil || !strings.HasSuffix(string(requests), "https://github.com/arizzi74/MACT/releases/download/v0.7.10/"+target.ManagerAsset()+"\n") {
				t.Fatalf("wrong manager asset: %s %v", requests, err)
			}
		})
	}
}

func TestBootstrapRejectsUnsupportedPlatformAndMissingChecksumUtility(t *testing.T) {
	for _, scenario := range []string{"unsupported OS", "unsupported CPU", "missing checksum utility"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			switch scenario {
			case "unsupported OS":
				fixture.environment = append(fixture.environment, "TEST_OS=FreeBSD")
			case "unsupported CPU":
				fixture.environment = append(fixture.environment, "TEST_ARCH=riscv64")
			case "missing checksum utility":
				for _, name := range []string{"sha256sum", "shasum"} {
					if err := os.Remove(filepath.Join(fixture.bin, name)); err != nil && !os.IsNotExist(err) {
						t.Fatal(err)
					}
				}
			}
			if output, err := fixture.run("setup", "worker"); err == nil {
				t.Fatalf("accepted unavailable platform/verification: %s", output)
			}
			if _, err := os.Stat(filepath.Join(fixture.root, "requests")); !os.IsNotExist(err) {
				t.Fatalf("downloaded before requirements were checked: %v", err)
			}
		})
	}
}

// This verifier is installed by the trusted test harness before the optional
// manual procedure. It cryptographically authenticates local release fixtures
// with a separately held public key, while enforcing the gh invocation contract.
// It does not claim to replace integration testing of GitHub CLI's Sigstore code.
type bootstrapClaim struct {
	Repository, Identity, Issuer, Ref, Predicate, Digest string
}

type bootstrapEnvelope struct {
	Claim     json.RawMessage
	Signature []byte
}

// Only the optional manual guide uses a separately provisioned verifier. Its
// fixture authenticates local provenance with an independent test signing key.
func (fixture *bootstrapFixture) trustManualVerifier(t *testing.T) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(fixture.bin, "gh"), []byte("#!/bin/sh\nexec \"$TEST_VERIFIER\" -test.run=^TestBootstrapTrustedVerifierProcess$ -- \"$@\"\n"))
	if err := os.Chmod(filepath.Join(fixture.bin, "gh"), 0755); err != nil {
		t.Fatal(err)
	}
	fixture.key = private
	fixture.environment = append(fixture.environment, "TEST_VERIFIER="+verifier, "TEST_TRUSTED_VERIFIER=1", "TEST_VERIFIER_PUBLIC="+base64.StdEncoding.EncodeToString(public))
	fixture.sign(t, "arizzi74/MACT", "v1.2.3")
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

func TestDocumentedInstallerRejectsUnauthenticatedManagerBeforeExecution(t *testing.T) {
	command := documentedInstallerCommand(t)
	for _, component := range []string{"worker", "gateway"} {
		for _, scenario := range []string{"matching malicious checksums", "wrong repository", "wrong workflow", "wrong tag", "wrong source ref", "wrong issuer", "wrong predicate", "bad signature", "missing provenance", "malformed provenance", "interrupted manifest", "interrupted provenance", "interrupted manager", "missing verifier", "old verifier"} {
			t.Run(component+"/"+scenario, func(t *testing.T) {
				fixture := bootstrapTestFixture(t)
				fixture.trustManualVerifier(t)
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
				cmd := exec.Command("/bin/sh", "-c", command+"\n[ -n \"$MACT_INSTALLER_DIR\" ] || exit 1\n\"$MACT_INSTALLER_DIR/install\" setup "+component)
				cmd.Env = fixture.environment
				if output, err := cmd.CombinedOutput(); err == nil {
					t.Fatalf("accepted unauthenticated installer: %s", output)
				}
				if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
					t.Fatal("executed unauthenticated code")
				}
				remaining, err := filepath.Glob(filepath.Join(fixture.root, "mact-installer.*"))
				if err != nil || len(remaining) != 0 {
					t.Fatalf("left partial installation: %v %v", remaining, err)
				}
			})
		}
	}
}

func documentedInstallerCommand(t *testing.T) string {
	t.Helper()
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
	if strings.Count(command, "version=v0.7.10") != 1 {
		t.Fatal("missing explicit stable installer version")
	}
	command = strings.Replace(command, "version=v0.7.10", "version=v1.2.3", 1)
	return command
}

func TestDocumentedInstallerAuthenticatesBeforeExecution(t *testing.T) {
	command := documentedInstallerCommand(t)
	for _, scenario := range []string{"authenticated", "matching malicious checksums", "substituted manager", "wrong tag", "missing provenance", "interrupted download"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := bootstrapTestFixture(t)
			fixture.trustManualVerifier(t)
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
	if output, err := command.CombinedOutput(); err == nil || !strings.Contains(string(output), "installer checksum verification failed") {
		t.Fatalf("privileged installer did not reject substituted manager: %v %s", err, output)
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

func TestDocumentedInstallerRejectsVerifierWithoutRequiredPolicyFlags(t *testing.T) {
	fixture := bootstrapTestFixture(t)
	fixture.trustManualVerifier(t)
	// Some old verifiers have the command but lack exact source-ref policy.
	writeTestFile(t, filepath.Join(fixture.bin, "gh"), []byte("#!/bin/sh\n[ \"$3\" = --help ] && exit 0\nexit 1\n"))
	cmd := exec.Command("/bin/sh", "-c", documentedInstallerCommand(t))
	cmd.Env = fixture.environment
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("accepted unsupported verification policy: %s", output)
	}
	if _, err := os.Stat(filepath.Join(fixture.root, "record")); !os.IsNotExist(err) {
		t.Fatal("executed manager without complete publisher verification")
	}
}
