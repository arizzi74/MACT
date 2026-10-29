package releasemanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func claudeResponse(request *http.Request, data []byte) *http.Response {
	response := coreResponse(request, data)
	response.ContentLength = int64(len(data))
	return response
}

func claudeDistributionFixture(t *testing.T) (*Manager, string, string, string) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("native Claude updates support Unix installations")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("DISABLE_UPDATES", "")
	t.Setenv("DISABLE_AUTOUPDATER", "")
	launcher := filepath.Join(home, ".local", "bin", "claude")
	target := filepath.Join(home, ".local", "share", "claude", "versions", "2.1.310")
	for _, path := range []string{filepath.Dir(launcher), filepath.Dir(target)} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Use the host's native ELF/Mach-O layout, with all execution replaced by
	// the bounded Runner fixture. The real Claude CLI is never invoked here.
	var data []byte
	if runtime.GOOS == "darwin" {
		// A thin Mach-O executable header avoids the universal /bin/ls binary
		// and its unrelated architectures on macOS fixture hosts.
		data = make([]byte, 32)
		binary.LittleEndian.PutUint32(data, 0xfeedfacf)
		cpu := uint32(0x01000007)
		if runtime.GOARCH == "arm64" {
			cpu = 0x0100000c
		}
		binary.LittleEndian.PutUint32(data[4:], cpu)
		binary.LittleEndian.PutUint32(data[12:], 2)
	} else {
		data, err = os.ReadFile("/bin/ls")
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(target, data, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, launcher); err != nil {
		t.Fatal(err)
	}
	m := New(nil)
	m.Run = func(_ context.Context, args ...string) (CommandResult, error) {
		if !slices.Equal(args, []string{launcher, "--version"}) {
			t.Fatalf("unexpected Claude distribution command: %v", args)
		}
		return CommandResult{Output: []byte("2.1.310 (Claude Code)\n")}, nil
	}
	return m, launcher, home, target
}

func TestClaudeDistributionParsesOnlyStableNativeVersions(t *testing.T) {
	for _, input := range []string{"2.1.310", "2.1.310 (Claude Code)\n"} {
		if version, err := parseClaudeVersion(input); err != nil || version != "2.1.310" {
			t.Fatalf("parse %q = %q, %v", input, version, err)
		}
	}
	for _, input := range []string{"", "v2.1.310", "2.01.310", "2.1.310-beta.1", "2.1.310+local", "2.1.310\nsecret", "claude 2.1.310"} {
		if _, err := parseClaudeVersion(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestClaudeDistributionInspectsOwnedNativeLauncher(t *testing.T) {
	m, binary, home, target := claudeDistributionFixture(t)
	for _, path := range []string{filepath.Join(home, ".local"), filepath.Join(home, ".local", "share"), filepath.Dir(filepath.Dir(target)), filepath.Dir(target)} {
		if err := os.Chmod(path, 0775); err != nil {
			t.Fatal(err)
		}
	}
	distribution, err := m.inspectClaudeDistribution(t.Context(), binary)
	if err != nil {
		t.Fatal(err)
	}
	if distribution.Home != home || distribution.Version != "2.1.310" || distribution.Binary != binary || distribution.VersionsDir != filepath.Dir(target) || distribution.Channel != "latest" || !strings.HasPrefix(distribution.Platform, runtime.GOOS+"-") {
		t.Fatalf("distribution = %+v", distribution)
	}
}

func TestClaudeDistributionRejectsUnmanagedAndRedirectedPathsBeforeExecution(t *testing.T) {
	for _, kind := range []string{"direct-pin", "wrapper", "npm-link", "symlink-version", "symlink-ancestor", "foreign-home", "world-writable", "non-executable", "foreign-owner"} {
		t.Run(kind, func(t *testing.T) {
			m, binary, home, target := claudeDistributionFixture(t)
			switch kind {
			case "direct-pin":
				binary = target
			case "wrapper":
				if err := os.WriteFile(target, []byte("#!/bin/sh\necho 2.1.310 '(Claude Code)'\n"), 0755); err != nil {
					t.Fatal(err)
				}
			case "npm-link":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(home, "node_modules", "@anthropic-ai", "claude-code", "cli.js"), binary); err != nil {
					t.Fatal(err)
				}
			case "symlink-version":
				if err := os.Rename(target, target+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target+".old", target); err != nil {
					t.Fatal(err)
				}
			case "symlink-ancestor":
				path := filepath.Dir(target)
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".old", path); err != nil {
					t.Fatal(err)
				}
			case "foreign-home":
				t.Setenv("HOME", t.TempDir())
			case "world-writable":
				if err := os.Chmod(filepath.Dir(target), 0777); err != nil {
					t.Fatal(err)
				}
			case "non-executable":
				if err := os.Chmod(target, 0644); err != nil {
					t.Fatal(err)
				}
			case "foreign-owner":
				if os.Geteuid() != 0 {
					t.Skip("changing ownership requires root")
				}
				if err := os.Chown(target, 65534, 65534); err != nil {
					t.Fatal(err)
				}
			}
			m.Run = func(context.Context, ...string) (CommandResult, error) {
				t.Fatal("unsupported Claude installation was executed")
				return CommandResult{}, nil
			}
			if distribution, err := m.inspectClaudeDistribution(t.Context(), binary); distribution != nil || !errors.Is(err, ErrClaudeDistributionUnsupported) {
				t.Fatalf("inspect = %+v, %v", distribution, err)
			}
		})
	}
}

func TestClaudeDistributionRejectsVersionMismatchWithoutLeakingOutput(t *testing.T) {
	m, binary, _, _ := claudeDistributionFixture(t)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		return CommandResult{Output: []byte("secret-token /private/account")}, nil
	}
	if _, err := m.inspectClaudeDistribution(t.Context(), binary); err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "/private") {
		t.Fatalf("inspect = %v", err)
	}
}

func TestClaudeDistributionFetchesOfficialConfiguredChannel(t *testing.T) {
	for _, channel := range []string{"latest", "stable"} {
		t.Run(channel, func(t *testing.T) {
			m := New(nil)
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != claudeReleaseBaseURL+"/"+channel || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("User-Agent") == "" {
					t.Fatalf("unexpected official channel request: %s", r.URL)
				}
				return claudeResponse(r, []byte("2.1.312\n")), nil
			})
			if version, err := m.fetchClaudeChannel(t.Context(), channel); err != nil || version != "2.1.312" {
				t.Fatalf("channel = %q, %v", version, err)
			}
		})
	}
}

func TestClaudeDistributionRejectsUnsafeMetadataAndRedirects(t *testing.T) {
	for _, kind := range []string{"oversize", "prerelease", "version-output", "transport", "server", "http-redirect", "foreign-redirect", "query-redirect", "path-redirect"} {
		t.Run(kind, func(t *testing.T) {
			m := New(nil)
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != claudeLatestURL {
					t.Fatalf("unsafe redirect followed: %s", r.URL)
				}
				response := claudeResponse(r, []byte("2.1.312"))
				switch kind {
				case "oversize":
					response = claudeResponse(r, bytes.Repeat([]byte("x"), 129))
				case "prerelease":
					response = claudeResponse(r, []byte("2.1.312-beta.1"))
				case "version-output":
					response = claudeResponse(r, []byte("2.1.312 (Claude Code)"))
				case "transport":
					return nil, errors.New("https://secret.example/token")
				case "server":
					response.StatusCode = http.StatusForbidden
				default:
					location := map[string]string{"http-redirect": "http://downloads.claude.ai/claude-code-releases/latest", "foreign-redirect": "https://secret.example/token", "query-redirect": claudeLatestURL + "?token=secret", "path-redirect": "https://downloads.claude.ai/other/latest"}[kind]
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", location)
				}
				return response, nil
			})
			if _, err := m.fetchClaudeChannel(t.Context(), "latest"); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe metadata = %v", err)
			}
		})
	}
}

func TestClaudeDistributionStagesVerifiedBinaryPrivatelyWithoutChangingLauncher(t *testing.T) {
	m, binary, _, target := claudeDistributionFixture(t)
	distribution, err := m.inspectClaudeDistribution(t.Context(), binary)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256(data)
	entry := claudePlatformRelease{Binary: "claude", Checksum: hex.EncodeToString(checksum[:]), Size: int64(len(data))}
	manifest, _ := json.Marshal(map[string]any{"version": "2.1.312", "platforms": map[string]claudePlatformRelease{distribution.Platform: entry}})
	signatures := 0
	m.claudeVerifyManifest = func(manifestInput, signature []byte) error {
		signatures++
		if !bytes.Equal(manifestInput, manifest) || string(signature) != "signed-manifest" {
			t.Fatal("manifest or signature changed")
		}
		return nil
	}
	m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case claudeReleaseBaseURL + "/2.1.312/manifest.json":
			return claudeResponse(r, manifest), nil
		case claudeReleaseBaseURL + "/2.1.312/manifest.json.sig":
			return claudeResponse(r, []byte("signed-manifest")), nil
		case claudeReleaseBaseURL + "/2.1.312/" + distribution.Platform + "/claude":
			return claudeResponse(r, data), nil
		default:
			t.Fatalf("unexpected release download: %s", r.URL)
			return nil, errors.New("unexpected")
		}
	})
	candidate, err := m.stageClaudeRuntime(t.Context(), distribution, "2.1.312")
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Cleanup()
	if signatures != 1 || filepath.Dir(candidate.Directory) != filepath.Dir(target) || candidate.Version != "2.1.312" || candidate.SHA256 != entry.Checksum {
		t.Fatalf("candidate = %+v, signatures=%d", candidate, signatures)
	}
	for _, path := range []string{candidate.Directory, candidate.Binary} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private stage permissions: %v, %v", info, err)
		}
	}
	if link, err := os.Readlink(binary); err != nil || link != target {
		t.Fatalf("native launcher changed: %q, %v", link, err)
	}
	if _, err := os.Stat(filepath.Join(distribution.VersionsDir, "2.1.312")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage published a native release: %v", err)
	}
}

func TestClaudeDistributionRejectsBadManifestAndBinaryAndCleansStage(t *testing.T) {
	for _, kind := range []string{"signature", "manifest-version", "platform", "hash", "size", "truncated", "oversize", "wrapper", "stale-installation", "same-version-replaced", "downgrade"} {
		t.Run(kind, func(t *testing.T) {
			m, binary, _, target := claudeDistributionFixture(t)
			distribution, err := m.inspectClaudeDistribution(t.Context(), binary)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "wrapper" {
				data = []byte("#!/bin/sh\necho secret-token\n")
			}
			checksum := sha256.Sum256(data)
			entry := claudePlatformRelease{Binary: "claude", Checksum: hex.EncodeToString(checksum[:]), Size: int64(len(data))}
			version, platform := "2.1.312", distribution.Platform
			switch kind {
			case "manifest-version":
				version = "2.1.311"
			case "platform":
				platform = "win32-x64"
			case "hash":
				entry.Checksum = strings.Repeat("0", 64)
			case "size":
				entry.Size = claudeBinaryLimit + 1
			case "truncated":
				data = data[:len(data)-1]
			case "oversize":
				data = append(data, 'x')
			case "stale-installation":
				distribution.Version = "2.1.309"
			case "same-version-replaced":
				if err := os.Rename(target, target+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, data, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(target + ".old"); err != nil {
					t.Fatal(err)
				}
			}
			manifest, _ := json.Marshal(map[string]any{"version": version, "platforms": map[string]claudePlatformRelease{platform: entry}})
			m.claudeVerifyManifest = func([]byte, []byte) error {
				if kind == "signature" {
					return errors.New("secret-token")
				}
				return nil
			}
			m.HTTP.Transport = coreRoundTripper(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/manifest.json") {
					return claudeResponse(r, manifest), nil
				}
				if strings.HasSuffix(r.URL.Path, ".sig") {
					return claudeResponse(r, []byte("signature")), nil
				}
				return claudeResponse(r, data), nil
			})
			requested := "2.1.312"
			if kind == "downgrade" {
				requested = "2.1.309"
			}
			candidate, err := m.stageClaudeRuntime(t.Context(), distribution, requested)
			if err == nil || candidate != nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("stage = %+v, %v", candidate, err)
			}
			entries, err := os.ReadDir(filepath.Dir(target))
			if err != nil || len(entries) != 1 || entries[0].Name() != "2.1.310" {
				t.Fatalf("failed stage leaked files: %v, %v", entries, err)
			}
		})
	}
}

func TestClaudeDistributionPolicyHonorsChannelAndVersionBounds(t *testing.T) {
	for _, input := range []struct{ settings, channel, minimum, maximum string }{
		{`{}`, "latest", "", ""},
		{`{"autoUpdatesChannel":"stable","minimumVersion":"2.1.310"}`, "stable", "2.1.310", ""},
		{`{"minimumVersion":"2.1.310","requiredMinimumVersion":"2.1.311","requiredMaximumVersion":"2.1.312"}`, "latest", "2.1.311", "2.1.312"},
	} {
		t.Run(input.settings, func(t *testing.T) {
			_, _, home, _ := claudeDistributionFixture(t)
			if err := os.Mkdir(filepath.Join(home, ".claude"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(input.settings), 0600); err != nil {
				t.Fatal(err)
			}
			policy, err := readClaudeUpdatePolicy(home)
			if err != nil || policy.Channel != input.channel || policy.MinimumVersion != input.minimum || policy.MaximumVersion != input.maximum {
				t.Fatalf("policy=%+v, %v", policy, err)
			}
		})
	}
}

func TestClaudeDistributionPolicyFailsClosed(t *testing.T) {
	for _, kind := range []string{"malformed", "prerelease", "channel", "conflicting", "disabled", "remote-policy", "symlink-settings", "oversize", "custom-config"} {
		t.Run(kind, func(t *testing.T) {
			_, _, home, _ := claudeDistributionFixture(t)
			dir := filepath.Join(home, ".claude")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			settings := map[string]string{"malformed": "secret-token", "prerelease": `{"minimumVersion":"2.1.312-beta.1"}`, "channel": `{"autoUpdatesChannel":"custom"}`, "conflicting": `{"minimumVersion":"2.1.312","requiredMaximumVersion":"2.1.310"}`, "disabled": `{"env":{"DISABLE_UPDATES":"1"}}`}[kind]
			if settings == "" {
				settings = `{}`
			}
			path := filepath.Join(dir, "settings.json")
			if kind == "remote-policy" {
				path = filepath.Join(dir, "remote-settings.json")
			}
			if kind == "oversize" {
				settings = strings.Repeat("x", claudeMetadataLimit+1)
			}
			if err := os.WriteFile(path, []byte(settings), 0600); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink-settings" {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".old", path); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "custom-config" {
				t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			}
			if _, err := readClaudeUpdatePolicy(home); err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatalf("unsafe policy=%v", err)
			}
		})
	}
}

func TestClaudeDistributionPinnedSigningKeyRejectsUnsignedManifest(t *testing.T) {
	if err := verifyClaudeManifest([]byte(`{"version":"2.1.312"}`), []byte("secret-token")); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("signature=%v", err)
	}
}

func TestClaudeDistributionNativePlatformPreservesArchitectureAndLibc(t *testing.T) {
	for _, input := range []struct {
		arch, interpreter, expected string
		machine                     uint16
	}{
		{"amd64", "/lib64/ld-linux-x86-64.so.2", "linux-x64", 62},
		{"amd64", "/lib/ld-musl-x86_64.so.1", "linux-x64-musl", 62},
		{"arm64", "/lib/ld-linux-aarch64.so.1", "linux-arm64", 183},
		{"arm64", "/lib/ld-musl-aarch64.so.1", "linux-arm64-musl", 183},
		{"amd64", "/lib/ld-linux-aarch64.so.1", "", 183},
		{"amd64", "/lib/custom-loader", "", 62},
		{"amd64", "ld-linux-x86-64.so.2", "", 62},
		{"386", "/lib64/ld-linux-x86-64.so.2", "", 62},
	} {
		t.Run(input.arch+input.interpreter, func(t *testing.T) {
			data := make([]byte, 120+len(input.interpreter)+1)
			copy(data, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
			binary.LittleEndian.PutUint16(data[16:], 2)
			binary.LittleEndian.PutUint16(data[18:], input.machine)
			binary.LittleEndian.PutUint32(data[20:], 1)
			binary.LittleEndian.PutUint64(data[32:], 64)
			binary.LittleEndian.PutUint16(data[52:], 64)
			binary.LittleEndian.PutUint16(data[54:], 56)
			binary.LittleEndian.PutUint16(data[56:], 1)
			binary.LittleEndian.PutUint32(data[64:], 3)
			binary.LittleEndian.PutUint64(data[72:], 120)
			binary.LittleEndian.PutUint64(data[96:], uint64(len(input.interpreter)+1))
			copy(data[120:], input.interpreter)
			path := filepath.Join(t.TempDir(), "claude")
			if err := os.WriteFile(path, data, 0700); err != nil {
				t.Fatal(err)
			}
			platform, err := claudeNativePlatform(path, "linux", input.arch)
			if platform != input.expected || (err != nil) != (input.expected == "") {
				t.Fatalf("platform=%q, %v", platform, err)
			}
		})
	}
}

func TestClaudeDistributionPolicyHonorsDisabledEnvironmentWithoutSettings(t *testing.T) {
	for _, key := range []string{"DISABLE_UPDATES", "DISABLE_AUTOUPDATER"} {
		t.Run(key, func(t *testing.T) {
			_, _, home, _ := claudeDistributionFixture(t)
			t.Setenv(key, "1")
			if _, err := readClaudeUpdatePolicy(home); err == nil {
				t.Fatal("ignored native update disable environment")
			}
		})
	}
}
