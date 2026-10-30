package releasemanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// These are the public native distribution endpoints documented at
// https://code.claude.com/docs/en/setup#binary-integrity-and-code-signing.
// Package managers, the legacy Google bucket and custom mirrors are not used.
const claudeReleaseBaseURL = "https://downloads.claude.ai/claude-code-releases"
const claudeLatestURL = claudeReleaseBaseURL + "/latest"
const claudeMetadataLimit = 2 * 1024 * 1024
const claudeBinaryLimit int64 = 512 * 1024 * 1024
const claudeUpdateTimeout = 10 * time.Minute

var ErrClaudeDistributionUnsupported = errors.New("automatic Claude runtime updates require a supported, user-owned native installation")

type claudeDistribution struct {
	Binary, Home, InstallDir, VersionsDir, Version, Platform string
	Channel, MinimumVersion, MaximumVersion                  string
	Resolved, LinkTarget                                     string
	Device, Inode                                            uint64
	Size                                                     int64
	ModifiedAt                                               time.Time
}

// The staged candidate is private and never changes the native launcher.
// Its caller owns promotion and must remove Directory on every exit path.
type claudeStagedRuntime struct {
	Binary, Directory, Version, SHA256 string
}

func (candidate *claudeStagedRuntime) Cleanup() error {
	if candidate == nil || candidate.Directory == "" {
		return nil
	}
	return os.RemoveAll(candidate.Directory)
}

func parseClaudeVersion(output string) (string, error) {
	version := strings.TrimSuffix(strings.TrimSpace(output), " (Claude Code)")
	if _, err := ParseVersion(version); err != nil || strings.HasPrefix(version, "v") {
		return "", errors.New("Claude runtime version must be a stable MAJOR.MINOR.PATCH version")
	}
	return version, nil
}

// Every request, including redirects, stays on the official HTTPS release
// subtree. Neither transport failures nor remote response bodies reach logs.
func (m *Manager) claudeReleaseRequest(ctx context.Context, source string) (*http.Response, error) {
	allowed := func(u *url.URL) bool {
		return u != nil && u.Scheme == "https" && u.Host == "downloads.claude.ai" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.HasPrefix(u.EscapedPath(), "/claude-code-releases/") && !strings.Contains(u.EscapedPath(), "%") && !strings.Contains(u.Path, "..")
	}
	u, err := url.Parse(source)
	if err != nil || !allowed(u) {
		return nil, errors.New("invalid official Claude release URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, errors.New("cannot construct the Claude release request")
	}
	req.Header.Set("User-Agent", "mact-release-manager")
	req.Header.Set("Accept", "application/octet-stream")
	client := *m.HTTP
	client.Jar = nil
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !allowed(req.URL) {
			return errors.New("rejected an unexpected Claude release redirect")
		}
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Claude release request failed")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("Claude release request failed (HTTP %d)", response.StatusCode)
	}
	return response, nil
}

func (m *Manager) fetchClaudeMetadata(ctx context.Context, source string, limit int64) ([]byte, error) {
	response, err := m.claudeReleaseRequest(ctx, source)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit || response.ContentLength > int64(len(data)) {
		return nil, errors.New("cannot read Claude release metadata within its size limit")
	}
	return data, nil
}

func (m *Manager) fetchClaudeLatest(ctx context.Context) (string, error) {
	return m.fetchClaudeChannel(ctx, "latest")
}

func (m *Manager) fetchClaudeChannel(ctx context.Context, channel string) (string, error) {
	if channel != "latest" && channel != "stable" {
		return "", errors.New("unsupported Claude runtime update channel")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	data, err := m.fetchClaudeMetadata(ctx, claudeReleaseBaseURL+"/"+channel, 128)
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(data))
	if _, err = parseClaudeVersion(version); err != nil || strings.ContainsAny(version, " \t\r\n") {
		return "", errors.New("invalid stable Claude release channel metadata")
	}
	return version, nil
}

func claudeOwnedPath(path string, directory bool) bool {
	return codexOwnedPath(path, directory)
}

func claudeOwnedLink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

// Only the documented native launcher in this user's home is managed. Direct
// version paths, npm packages, wrappers and redirected ancestors stay untouched.
func (m *Manager) inspectClaudeDistribution(ctx context.Context, executable string) (*claudeDistribution, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, ErrClaudeDistributionUnsupported
	}
	home = filepath.Clean(home)
	policy, err := m.readClaudeUpdatePolicy(home)
	if err != nil {
		return nil, err
	}
	binary, err := exec.LookPath(executable)
	if err != nil {
		return nil, ErrClaudeDistributionUnsupported
	}
	binary, err = filepath.Abs(binary)
	installDir := filepath.Join(home, ".local", "bin")
	versionsDir := filepath.Join(home, ".local", "share", "claude", "versions")
	if err != nil || binary != filepath.Join(installDir, "claude") || !claudeOwnedLink(binary) {
		return nil, ErrClaudeDistributionUnsupported
	}
	for _, dir := range []string{home, filepath.Join(home, ".local"), installDir, filepath.Join(home, ".local", "share"), filepath.Dir(versionsDir), versionsDir} {
		if !codexOwnedPath(dir, true) {
			return nil, ErrClaudeDistributionUnsupported
		}
	}
	target, err := os.Readlink(binary)
	if err != nil {
		return nil, ErrClaudeDistributionUnsupported
	}
	linkTarget := target
	if !filepath.IsAbs(target) {
		target = filepath.Join(installDir, target)
	}
	target = filepath.Clean(target)
	version := filepath.Base(target)
	parsed, err := parseClaudeVersion(version)
	if err != nil || parsed != version || filepath.Dir(target) != versionsDir || !codexOwnedPath(target, false) {
		return nil, ErrClaudeDistributionUnsupported
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode().Perm()&0111 == 0 {
		return nil, ErrClaudeDistributionUnsupported
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, ErrClaudeDistributionUnsupported
	}
	platform, err := claudeNativePlatform(target, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, ErrClaudeDistributionUnsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	output, err := m.command(ctx, binary, "--version")
	if err != nil {
		return nil, errors.New("cannot inspect the installed Claude runtime version")
	}
	actual, err := parseClaudeVersion(string(output))
	if err != nil || actual != version {
		return nil, errors.New("installed Claude runtime version does not match its native release path")
	}
	after, statErr := os.Lstat(target)
	afterLink, linkErr := os.Readlink(binary)
	resolved, resolveErr := filepath.EvalSymlinks(binary)
	if statErr != nil || linkErr != nil || resolveErr != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || info.Mode() != after.Mode() || afterLink != linkTarget || resolved != target || !claudeOwnedLink(binary) || !claudeOwnedPath(target, false) {
		return nil, errors.New("Claude installation changed while inspecting its native release; retry later")
	}
	return &claudeDistribution{Binary: binary, Home: home, InstallDir: installDir, VersionsDir: versionsDir, Version: version, Platform: platform, Channel: policy.Channel, MinimumVersion: policy.MinimumVersion, MaximumVersion: policy.MaximumVersion, Resolved: target, LinkTarget: linkTarget, Device: uint64(stat.Dev), Inode: uint64(stat.Ino), Size: info.Size(), ModifiedAt: info.ModTime().UTC()}, nil
}

// Preserve the installed native binary's libc variant rather than guessing
// from ldd output, PATH commands, or an unrelated musl installation.
func claudeNativePlatform(binary, system, arch string) (string, error) {
	architecture := map[string]string{"amd64": "x64", "arm64": "arm64"}[arch]
	if architecture == "" {
		return "", ErrClaudeDistributionUnsupported
	}
	if system == "darwin" {
		file, err := macho.Open(binary)
		if err != nil {
			return "", ErrClaudeDistributionUnsupported
		}
		defer file.Close()
		if file.Type != macho.TypeExec || (arch == "amd64" && file.Cpu != macho.CpuAmd64) || (arch == "arm64" && file.Cpu != macho.CpuArm64) {
			return "", ErrClaudeDistributionUnsupported
		}
		return "darwin-" + architecture, nil
	}
	if system != "linux" {
		return "", ErrClaudeDistributionUnsupported
	}
	file, err := elf.Open(binary)
	if err != nil {
		return "", ErrClaudeDistributionUnsupported
	}
	defer file.Close()
	if file.Class != elf.ELFCLASS64 || file.Data != elf.ELFDATA2LSB || (file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN) || (arch == "amd64" && file.Machine != elf.EM_X86_64) || (arch == "arm64" && file.Machine != elf.EM_AARCH64) {
		return "", ErrClaudeDistributionUnsupported
	}
	for _, program := range file.Progs {
		if program.Type != elf.PT_INTERP {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(program.Open(), 4097))
		if err != nil || len(data) == 0 || len(data) > 4096 || data[len(data)-1] != 0 {
			return "", ErrClaudeDistributionUnsupported
		}
		interpreter := string(data[:len(data)-1])
		if !filepath.IsAbs(interpreter) {
			return "", ErrClaudeDistributionUnsupported
		}
		name := filepath.Base(interpreter)
		if (arch == "amd64" && name == "ld-musl-x86_64.so.1") || (arch == "arm64" && name == "ld-musl-aarch64.so.1") {
			return "linux-" + architecture + "-musl", nil
		}
		if (arch == "amd64" && name == "ld-linux-x86-64.so.2") || (arch == "arm64" && name == "ld-linux-aarch64.so.1") {
			return "linux-" + architecture, nil
		}
		return "", ErrClaudeDistributionUnsupported
	}
	return "", ErrClaudeDistributionUnsupported
}

type claudePlatformRelease struct {
	Binary   string `json:"binary"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

func parseClaudeManifest(data []byte, version, platform string) (claudePlatformRelease, error) {
	var manifest struct {
		Version   string                           `json:"version"`
		Platforms map[string]claudePlatformRelease `json:"platforms"`
	}
	if json.Unmarshal(data, &manifest) != nil || manifest.Version != version {
		return claudePlatformRelease{}, errors.New("invalid Claude release manifest version")
	}
	entry, exists := manifest.Platforms[platform]
	checksum, err := hex.DecodeString(entry.Checksum)
	if !exists || entry.Binary != "claude" || err != nil || len(checksum) != sha256.Size || strings.ToLower(entry.Checksum) != entry.Checksum || entry.Size <= 0 || entry.Size > claudeBinaryLimit {
		return claudePlatformRelease{}, errors.New("invalid Claude release manifest platform, size or checksum")
	}
	return entry, nil
}

func verifyClaudeManifest(manifest, signature []byte) error {
	keys, err := openpgp.ReadArmoredKeyRing(strings.NewReader(claudeReleaseSigningKey))
	if err != nil || len(keys) != 1 || hex.EncodeToString(keys[0].PrimaryKey.Fingerprint[:]) != "31ddde24ddfab679f42d7bd2baa929ff1a7ecace" {
		return errors.New("invalid pinned Claude release signing key")
	}
	if _, err = openpgp.CheckArmoredDetachedSignature(keys, bytes.NewReader(manifest), bytes.NewReader(signature), nil); err != nil {
		return errors.New("Claude release manifest signature verification failed")
	}
	return nil
}

func (m *Manager) stageClaudeRuntime(ctx context.Context, distribution *claudeDistribution, version string) (_ *claudeStagedRuntime, retErr error) {
	if distribution == nil {
		return nil, ErrClaudeDistributionUnsupported
	}
	parsed, err := parseClaudeVersion(version)
	if err != nil || parsed != version {
		return nil, errors.New("invalid Claude runtime update target")
	}
	if comparison, err := CompareVersions(version, "2.1.89"); err != nil || comparison < 0 {
		return nil, errors.New("Claude runtime updates require a release with a signed manifest")
	}
	current, err := m.inspectClaudeDistribution(ctx, distribution.Binary)
	if err != nil || *current != *distribution {
		return nil, errors.New("Claude installation changed while preparing its update; retry later")
	}
	if comparison, _ := CompareVersions(version, distribution.Version); comparison <= 0 {
		return nil, errors.New("Claude runtime staging requires a newer release")
	}
	if distribution.MinimumVersion != "" {
		if comparison, _ := CompareVersions(version, distribution.MinimumVersion); comparison < 0 {
			return nil, errors.New("Claude runtime release is below its configured minimum version")
		}
	}
	if distribution.MaximumVersion != "" {
		if comparison, _ := CompareVersions(version, distribution.MaximumVersion); comparison > 0 {
			return nil, errors.New("Claude runtime release exceeds its configured maximum version")
		}
	}
	if err := cleanupClaudeRuntimeStages(distribution.VersionsDir, m.updateNow()); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, claudeUpdateTimeout)
	defer cancel()
	releaseURL := claudeReleaseBaseURL + "/" + version
	manifest, err := m.fetchClaudeMetadata(ctx, releaseURL+"/manifest.json", claudeMetadataLimit)
	if err != nil {
		return nil, err
	}
	signature, err := m.fetchClaudeMetadata(ctx, releaseURL+"/manifest.json.sig", 64*1024)
	if err != nil {
		return nil, err
	}
	verify := verifyClaudeManifest
	if m.claudeVerifyManifest != nil {
		verify = m.claudeVerifyManifest
	}
	if err = verify(manifest, signature); err != nil {
		return nil, errors.New("Claude release manifest signature verification failed")
	}
	entry, err := parseClaudeManifest(manifest, version, distribution.Platform)
	if err != nil {
		return nil, err
	}
	current, err = m.inspectClaudeDistribution(ctx, distribution.Binary)
	if err != nil || *current != *distribution {
		return nil, errors.New("Claude installation changed while downloading its release metadata; retry later")
	}
	dir, err := os.MkdirTemp(distribution.VersionsDir, ".mact-stage-")
	if err != nil {
		return nil, errors.New("cannot create private Claude runtime staging directory")
	}
	defer func() {
		if retErr != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	path := filepath.Join(dir, "claude")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("cannot create staged Claude runtime")
	}
	defer file.Close()
	response, err := m.claudeReleaseRequest(ctx, releaseURL+"/"+distribution.Platform+"/claude")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.ContentLength >= 0 && response.ContentLength != entry.Size {
		return nil, errors.New("Claude runtime download does not match its manifest size")
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, entry.Size+1))
	if err != nil || written != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.Checksum {
		return nil, errors.New("Claude runtime download failed its size or checksum verification")
	}
	if err = file.Chmod(0700); err != nil {
		return nil, errors.New("cannot protect staged Claude runtime permissions")
	}
	if err = file.Sync(); err != nil {
		return nil, errors.New("cannot persist staged Claude runtime")
	}
	if err = file.Close(); err != nil {
		return nil, errors.New("cannot close staged Claude runtime")
	}
	if platform, err := claudeNativePlatform(path, runtime.GOOS, runtime.GOARCH); err != nil || platform != distribution.Platform {
		return nil, errors.New("staged Claude runtime does not match the installed native platform")
	}
	current, err = m.inspectClaudeDistribution(ctx, distribution.Binary)
	if err != nil || *current != *distribution {
		return nil, errors.New("Claude installation changed while staging its update; retry later")
	}
	return &claudeStagedRuntime{Binary: path, Directory: dir, Version: version, SHA256: entry.Checksum}, nil
}

// Official key, pinned to the fingerprint independently documented in setup.
const claudeReleaseSigningKey = `-----BEGIN PGP PUBLIC KEY BLOCK-----

mQINBGnK73ABEACnbytJXkjweYrwIr0aLEFRlH+C0nF44KxFc7gQmJ6PjSPMGZAD
dxZcaixU7zZl8WxEpVO0wLmIH8cf2zGOdyuZg1Yaugk1vHb2b8WBhAGCQJdPgB8W
XquedepEYtk56uP/gCoTjJDUZluEGBHnlnuujSJ4orxEdhSykEoAUfJZGEILPpMd
bphFt/Sn+Eb/TxM5jpKPdwnv8AShNF/1mZU1fWTQq9tRKJUakZj04gdaDFElQXak
CtTij+GT6yoYCARSHwGO+PC/Pr6q4tc+D7LRjxSBvUWDoFSmlqb/PJ1hj9D/7I2O
e4XXniAPWMR56KvxHlzOzrNQdJujbJdSkCwh1ZijkSd3y8ayW5WYUTGdRab99NUw
agzlabe/VVF6kzJ0Scn5q3PihB2Y9Bwo0CKnkYk7a7KT77EWv0Kkq+VHmOtqX3a2
hhX+b6a6ve9rzJ1qZYGj+obv/C3Sx1LzUjAfqVy7RJDf2uAoP5t2g8u/TkSpUxhM
VEjZBkSxYZhMyzQM6t8IgkUfnSrIPTHixbDWARZ4beMOBjxyPZK1nP7OOrNR3TkK
JtwLMQAabURCDnL0PjS0iwBTU4jtumBD1XSULyWuoTvMljrpQr1nV1oDyOt0OLqa
KA2McWtd9PdXhC8y2EIg7TmrTlJLfHYbdmkiCYj4J49Q8HWkN/6WE+RTUwARAQAB
tD5BbnRocm9waWMgQ2xhdWRlIENvZGUgUmVsZWFzZSBTaWduaW5nIDxzZWN1cml0
eUBhbnRocm9waWMuY29tPokCUQQTAQoAOxYhBDHd3iTd+rZ59C170rqpKf8afsrO
BQJpyu9wAhsPBQsJCAcCAiICBhUKCQgLAgQWAgMBAh4HAheAAAoJELqpKf8afsrO
l5IP/2I8X1dFy5xYczWB/coIxGjuzS/V6ByZGZZEJsbr04pmuHiFUykJqPGWGQ6q
U0YF5iEwvEkaagS5m7DzhSEf3FM3Cgafax/6d70tar9Vr1D+w6uPfxetu7u/WYJp
aolIsdh5fTrBh9zSM1Njl8FM8wG8CwZQjS33Oa7d8cwRkgdUWbt6LXgz+cTQNuBn
BgW6Ks7oZFI25dfu0ojDR+aDFJg4+4wZoyDLPvJz1SIrJ5WFGs67zsx9SfS3yZnf
XKmBe+f0dUy+GJ2nFZrXFf99+c0dPEHYO8DCeAHZizjkFrdYtUHdDU0YDYEGkLJa
bE+pgcpkHf5EvsZzHsyDbl95W/eh7pcXMbwkN+W4CBYUE9X4uHhqzWaC5yAVRWUA
1BJ9V4LjZfHPLEJt0I3TxzXiEg9/BVeaTYq9RjaxIFo9Nfk158HqJY6SA5jslBlx
Gv/No8u+xVcze2UJyGVfEIUfm92+0UAIkny3+5cuVV0ICzJxXlXj0CnLM9Lt50wE
p3suVwuBEviCbZ08eAH1Ht8gbBdSsiOkIU8CX3v/scwHHx5q0+NBL6xLrQObg13a
tRXBlKObfElkPN3lTUbUnJOW4U8uSjH8VRP+AujKWMDFe7x0zCs+iYY1mTOvbrTS
9n3CmZUmbynZ+E/QWNENpW/pDNZdWFy43PASmML5FHu4m9Sn
=oqMI
-----END PGP PUBLIC KEY BLOCK-----
`
