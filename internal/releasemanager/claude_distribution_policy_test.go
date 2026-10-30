package releasemanager

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A remote policy is the settings object itself, rather than a response wrapper.
// These model and permission controls do not restrict native runtime updates.
const unrelatedClaudeRemotePolicy = `{
  "availableModels": ["claude-sonnet-4-6", "claude-opus-4-6"],
  "companyAnnouncements": [{"announcement": "Company Claude usage policy"}],
  "enforceAvailableModels": true,
  "forceRemoteSettingsRefresh": true,
  "model": "claude-sonnet-4-6",
  "permissions": {
    "deny": ["Read(./private/**)", "WebFetch(domain:private.example)"],
    "disableBypassPermissionsMode": "disable"
  }
}`

func claudePolicySourcesFixture(t *testing.T) claudeUpdatePolicySources {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return claudeUpdatePolicySources{ManagedRoot: root}
}

func clearClaudeProviderRoutingFixture(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS", "ANTHROPIC_BASE_URL",
	} {
		t.Setenv(key, "")
	}
}

func claudePolicyFixture(t *testing.T) (string, claudeUpdatePolicySources) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DISABLE_UPDATES", "")
	t.Setenv("DISABLE_AUTOUPDATER", "")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	clearClaudeProviderRoutingFixture(t)
	return home, claudePolicySourcesFixture(t)
}

func writeClaudePolicyFixture(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func snapshotClaudePolicyFiles(t *testing.T, paths ...string) map[string][]byte {
	t.Helper()
	snapshot := make(map[string][]byte, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		snapshot[path] = data
	}
	return snapshot
}

func assertClaudePolicyFilesUnchanged(t *testing.T, snapshot map[string][]byte) {
	t.Helper()
	for path, before := range snapshot {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("policy or credentials were changed: %s (read error: %v)", filepath.Base(path), err)
		}
	}
}

func TestClaudeDistributionUnrelatedManagedPolicyPermitsNativeInspection(t *testing.T) {
	for _, source := range []string{"remote-settings.json", "managed-settings.json", "managed-mcp.json"} {
		t.Run(source, func(t *testing.T) {
			m, binary, home, _ := claudeDistributionFixture(t)
			path := filepath.Join(m.claudePolicySources.ManagedRoot, source)
			data := unrelatedClaudeRemotePolicy
			if source == "remote-settings.json" {
				path = filepath.Join(home, ".claude", source)
			}
			if source == "managed-mcp.json" {
				data = `{"mcpServers":{"private":{"command":"must-not-run","env":{"TOKEN":"private-mcp-token"}}}}`
			}
			credentialPath := filepath.Join(home, ".claude", ".credentials.json")
			writeClaudePolicyFixture(t, path, data)
			writeClaudePolicyFixture(t, credentialPath, `{"oauthToken":"private-credential-token"}`)
			before := snapshotClaudePolicyFiles(t, path, credentialPath)
			m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("inspection attempted a network request")
				return nil, errors.New("unexpected network request")
			})
			distribution, err := m.inspectClaudeDistribution(t.Context(), binary)
			if err != nil || distribution == nil {
				t.Fatalf("unrelated managed policy blocked native inspection: %v", err)
			}
			if distribution.Channel != "latest" || distribution.MinimumVersion != "" || distribution.MaximumVersion != "" {
				t.Fatalf("unrelated policy changed update controls: %+v", distribution)
			}
			assertClaudePolicyFilesUnchanged(t, before)
		})
	}
}

func TestClaudeUpdatePolicyManagedDisableControls(t *testing.T) {
	for _, source := range []string{"remote", "file", "drop-in"} {
		for _, key := range []string{"DISABLE_UPDATES", "DISABLE_AUTOUPDATER"} {
			for _, value := range []string{"1", "true", "YES", "On", "0", "false", ""} {
				t.Run(source+"/"+key+"/"+value, func(t *testing.T) {
					home, sources := claudePolicyFixture(t)
					path := filepath.Join(sources.ManagedRoot, "managed-settings.json")
					switch source {
					case "remote":
						path = filepath.Join(home, ".claude", "remote-settings.json")
					case "drop-in":
						path = filepath.Join(sources.ManagedRoot, "managed-settings.d", "10-updates.json")
					}
					writeClaudePolicyFixture(t, path, `{"env":{"`+key+`":"`+value+`"},"model":"private-model"}`)
					before := snapshotClaudePolicyFiles(t, path)
					_, err := readClaudeUpdatePolicyWithSources(home, sources)
					wantDisabled := value == "1" || value == "true" || value == "YES" || value == "On"
					if (err != nil) != wantDisabled {
						t.Fatalf("managed disable value %q: %v", value, err)
					}
					if err != nil && strings.Contains(err.Error(), "private-model") {
						t.Fatalf("policy error exposed managed payload: %v", err)
					}
					assertClaudePolicyFilesUnchanged(t, before)
				})
			}
		}
	}
}

func TestClaudeUpdatePolicyIgnoresUnrelatedSettingsAndEnvironment(t *testing.T) {
	home, sources := claudePolicyFixture(t)
	writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "settings.json"), `{"spinnerTipsEnabled":false,"env":{"NODE_VERSION":"22","PACKAGE_VERSION":"private-package-version"}}`)
	writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), `{"spinnerTipsEnabled":true,"env":{"PACKAGE_VERSION":"private-managed-package-version"}}`)
	if policy, err := readClaudeUpdatePolicyWithSources(home, sources); err != nil || policy != (claudeUpdatePolicy{Channel: "latest"}) {
		t.Fatalf("unrelated settings were treated as update controls: %+v, %v", policy, err)
	}
}

func TestClaudeUpdatePolicyManagedPrecedenceAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name, user, remote, managed string
		want                        claudeUpdatePolicy
		wantError                   bool
	}{
		{
			name: "user-required-bounds-are-managed-only",
			user: `{"autoUpdatesChannel":"stable","minimumVersion":"2.1.309","requiredMinimumVersion":"2.1.320","requiredMaximumVersion":"2.1.300"}`,
			want: claudeUpdatePolicy{Channel: "stable", MinimumVersion: "2.1.309"},
		},
		{
			name:    "managed-controls-override-user-controls",
			user:    `{"autoUpdatesChannel":"latest","minimumVersion":"2.1.320"}`,
			managed: `{"autoUpdatesChannel":"stable","minimumVersion":"2.1.310","requiredMinimumVersion":"2.1.311","requiredMaximumVersion":"2.1.315"}`,
			want:    claudeUpdatePolicy{Channel: "stable", MinimumVersion: "2.1.311", MaximumVersion: "2.1.315"},
		},
		{
			name:    "minimum-still-constrains-required-minimum",
			managed: `{"minimumVersion":"2.1.312","requiredMinimumVersion":"2.1.310","requiredMaximumVersion":"2.1.315"}`,
			want:    claudeUpdatePolicy{Channel: "latest", MinimumVersion: "2.1.312", MaximumVersion: "2.1.315"},
		},
		{
			name:    "highest-managed-source-wins-as-a-whole",
			user:    `{"autoUpdatesChannel":"stable"}`,
			remote:  unrelatedClaudeRemotePolicy,
			managed: `{"autoUpdatesChannel":"latest","requiredMinimumVersion":"2.1.320","requiredMaximumVersion":"2.1.300"}`,
			want:    claudeUpdatePolicy{Channel: "stable"},
		},
		{
			name:    "empty-remote-does-not-shadow-file-policy",
			remote:  `{}`,
			managed: `{"autoUpdatesChannel":"stable","requiredMinimumVersion":"2.1.311","requiredMaximumVersion":"2.1.315"}`,
			want:    claudeUpdatePolicy{Channel: "stable", MinimumVersion: "2.1.311", MaximumVersion: "2.1.315"},
		},
		{
			name:    "control-only-remote-does-not-shadow-file-policy",
			remote:  `{"managedSourcesBehavior":"first-wins","wslInheritsWindowsSettings":false}`,
			managed: `{"autoUpdatesChannel":"stable","requiredMaximumVersion":"2.1.315"}`,
			want:    claudeUpdatePolicy{Channel: "stable", MaximumVersion: "2.1.315"},
		},
		{
			name:    "remote-bounds-replace-lower-priority-bounds",
			remote:  `{"requiredMinimumVersion":"2.1.312","requiredMaximumVersion":"2.1.315"}`,
			managed: `{"requiredMinimumVersion":"2.1.320","requiredMaximumVersion":"2.1.310"}`,
			want:    claudeUpdatePolicy{Channel: "latest", MinimumVersion: "2.1.312", MaximumVersion: "2.1.315"},
		},
		{
			name:    "explicit-merge-falls-through-per-key",
			user:    `{"autoUpdatesChannel":"latest","minimumVersion":"2.1.309"}`,
			remote:  `{"managedSourcesBehavior":"merge","requiredMaximumVersion":"2.1.315"}`,
			managed: `{"autoUpdatesChannel":"stable","requiredMinimumVersion":"2.1.311","requiredMaximumVersion":"2.1.310"}`,
			want:    claudeUpdatePolicy{Channel: "stable", MinimumVersion: "2.1.311", MaximumVersion: "2.1.315"},
		},
		{
			name:    "first-wins-does-not-combine-conflicting-sources",
			remote:  `{"requiredMinimumVersion":"2.1.314"}`,
			managed: `{"requiredMaximumVersion":"2.1.313"}`,
			want:    claudeUpdatePolicy{Channel: "latest", MinimumVersion: "2.1.314"},
		},
		{
			name:      "merge-detects-conflict-across-sources",
			remote:    `{"managedSourcesBehavior":"merge","requiredMinimumVersion":"2.1.314"}`,
			managed:   `{"requiredMaximumVersion":"2.1.313"}`,
			wantError: true,
		},
		{
			name:      "selected-managed-policy-detects-conflict",
			remote:    `{"minimumVersion":"2.1.315","requiredMinimumVersion":"2.1.314","requiredMaximumVersion":"2.1.313"}`,
			wantError: true,
		},
		{
			name:      "selected-managed-null-cannot-clear-user-minimum",
			user:      `{"minimumVersion":"2.1.312"}`,
			remote:    `{"model":"managed-model","minimumVersion":null}`,
			wantError: true,
		},
		{
			name:   "all-null-remote-does-not-shadow-user-minimum",
			user:   `{"minimumVersion":"2.1.312"}`,
			remote: `{"minimumVersion":null}`,
			want:   claudeUpdatePolicy{Channel: "latest", MinimumVersion: "2.1.312"},
		},
		{
			name:    "user-cannot-select-managed-merge-behavior",
			user:    `{"managedSourcesBehavior":"merge"}`,
			remote:  `{"model":"managed-model"}`,
			managed: `{"requiredMaximumVersion":"2.1.313"}`,
			want:    claudeUpdatePolicy{Channel: "latest"},
		},
		{
			name:    "lower-managed-source-cannot-select-merge-behavior",
			remote:  `{"model":"managed-model"}`,
			managed: `{"managedSourcesBehavior":"merge","requiredMaximumVersion":"2.1.313"}`,
			want:    claudeUpdatePolicy{Channel: "latest"},
		},
		{
			name:      "file-disable-survives-unrelated-remote-source",
			user:      `{"env":{"DISABLE_UPDATES":"0"}}`,
			remote:    `{"model":"managed-model","env":{"UNRELATED_VARIABLE":"private-value"}}`,
			managed:   `{"env":{"DISABLE_UPDATES":"1"}}`,
			wantError: true,
		},
		{
			name:    "higher-managed-env-key-overrides-lower-key",
			user:    `{"env":{"DISABLE_UPDATES":"1"}}`,
			remote:  `{"env":{"DISABLE_UPDATES":"0"}}`,
			managed: `{"env":{"DISABLE_UPDATES":"1"}}`,
			want:    claudeUpdatePolicy{Channel: "latest"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			for path, data := range map[string]string{
				filepath.Join(home, ".claude", "settings.json"):             tc.user,
				filepath.Join(home, ".claude", "remote-settings.json"):      tc.remote,
				filepath.Join(sources.ManagedRoot, "managed-settings.json"): tc.managed,
			} {
				if data != "" {
					writeClaudePolicyFixture(t, path, data)
				}
			}
			policy, err := readClaudeUpdatePolicyWithSources(home, sources)
			if (err != nil) != tc.wantError || (!tc.wantError && policy != tc.want) {
				t.Fatalf("policy = %+v, error = %v; want %+v, error = %v", policy, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestClaudeUpdatePolicySettingsEnvironmentOverridesInheritedEnvironment(t *testing.T) {
	for _, key := range []string{"DISABLE_UPDATES", "DISABLE_AUTOUPDATER"} {
		for _, source := range []string{"user", "remote", "file"} {
			for _, value := range []string{"0", ""} {
				t.Run(key+"/"+source+"/"+value, func(t *testing.T) {
					home, sources := claudePolicyFixture(t)
					t.Setenv(key, "1")
					path := filepath.Join(home, ".claude", "settings.json")
					if source == "remote" {
						path = filepath.Join(home, ".claude", "remote-settings.json")
					}
					if source == "file" {
						path = filepath.Join(sources.ManagedRoot, "managed-settings.json")
					}
					writeClaudePolicyFixture(t, path, `{"env":{"`+key+`":"`+value+`"}}`)
					if policy, err := readClaudeUpdatePolicyWithSources(home, sources); err != nil || policy.Channel != "latest" {
						t.Fatalf("effective settings env did not override inherited env: %+v, %v", policy, err)
					}
				})
			}
		}
	}
}

func TestClaudeUpdatePolicyNonessentialTrafficDisablesUpdates(t *testing.T) {
	for _, source := range []string{"process", "user", "remote", "file"} {
		for _, value := range []string{"1", "0", "false", ""} {
			t.Run(source+"/"+value, func(t *testing.T) {
				home, sources := claudePolicyFixture(t)
				if source == "process" {
					t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", value)
				} else {
					path := filepath.Join(home, ".claude", "settings.json")
					if source == "remote" {
						path = filepath.Join(home, ".claude", "remote-settings.json")
					}
					if source == "file" {
						path = filepath.Join(sources.ManagedRoot, "managed-settings.json")
					}
					writeClaudePolicyFixture(t, path, `{"env":{"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":"`+value+`"}}`)
				}
				if _, err := readClaudeUpdatePolicyWithSources(home, sources); (err != nil) != (value != "") {
					t.Fatalf("nonessential traffic control value %q: %v", value, err)
				}
			})
		}
	}
}

func TestClaudeUpdatePolicyDropInsMergeInFilenameOrder(t *testing.T) {
	home, sources := claudePolicyFixture(t)
	paths := map[string]string{
		"managed-settings.json":              `{"autoUpdatesChannel":"latest","minimumVersion":"2.1.310","requiredMinimumVersion":"2.1.310","requiredMaximumVersion":"2.1.315","env":{"DISABLE_UPDATES":"1","DISABLE_AUTOUPDATER":"0"}}`,
		"managed-settings.d/20-last.json":    `{"autoUpdatesChannel":"stable","requiredMaximumVersion":"2.1.314","env":{"DISABLE_UPDATES":"0"}}`,
		"managed-settings.d/10-earlier.json": `{"requiredMinimumVersion":"2.1.311","env":{"UNRELATED_VARIABLE":"private-value"}}`,
		"managed-settings.d/.ignored.json":   `{"env":{"DISABLE_UPDATES":"1"}}`,
		"managed-settings.d/99-ignored.txt":  "not JSON and not a policy",
	}
	var files []string
	for relative, data := range paths {
		path := filepath.Join(sources.ManagedRoot, relative)
		writeClaudePolicyFixture(t, path, data)
		files = append(files, path)
	}
	before := snapshotClaudePolicyFiles(t, files...)
	policy, err := readClaudeUpdatePolicyWithSources(home, sources)
	want := claudeUpdatePolicy{Channel: "stable", MinimumVersion: "2.1.311", MaximumVersion: "2.1.314"}
	if err != nil || policy != want {
		t.Fatalf("drop-in policy = %+v, %v; want %+v", policy, err, want)
	}
	assertClaudePolicyFilesUnchanged(t, before)
}

func TestClaudeDistributionManagedVersionBoundsRejectBeforeDownload(t *testing.T) {
	for _, version := range []string{"2.1.311", "2.1.315"} {
		t.Run(version, func(t *testing.T) {
			m, binary, home, _ := claudeDistributionFixture(t)
			writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), `{"autoUpdatesChannel":"stable","requiredMinimumVersion":"2.1.312","requiredMaximumVersion":"2.1.314"}`)
			distribution, err := m.inspectClaudeDistribution(t.Context(), binary)
			if err != nil {
				t.Fatal(err)
			}
			if distribution.Channel != "stable" || distribution.MinimumVersion != "2.1.312" || distribution.MaximumVersion != "2.1.314" {
				t.Fatalf("managed bounds not carried into native distribution: %+v", distribution)
			}
			m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("out-of-policy release was downloaded")
				return nil, errors.New("unexpected download")
			})
			candidate, err := m.stageClaudeRuntime(t.Context(), distribution, version)
			if candidate != nil || err == nil {
				t.Fatalf("out-of-policy stage = %+v, %v", candidate, err)
			}
		})
	}
}

func TestClaudeDistributionPolicyChangeStopsStagingBeforeDownload(t *testing.T) {
	for _, policy := range []string{
		`{"env":{"DISABLE_UPDATES":"1"}}`,
		`{"env":{"DISABLE_AUTOUPDATER":"1"}}`,
		`{"requiredMaximumVersion":"2.1.311"}`,
	} {
		t.Run(policy, func(t *testing.T) {
			m, binary, home, _ := claudeDistributionFixture(t)
			distribution, err := m.inspectClaudeDistribution(t.Context(), binary)
			if err != nil {
				t.Fatal(err)
			}
			writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), policy)
			m.HTTP.Transport = coreRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("runtime was downloaded after native policy changed")
				return nil, errors.New("unexpected download")
			})
			candidate, err := m.stageClaudeRuntime(t.Context(), distribution, "2.1.312")
			if candidate != nil || err == nil {
				t.Fatalf("stage ignored changed policy: %+v, %v", candidate, err)
			}
		})
	}
}

func TestClaudeUpdatePolicyUnsafeManagedFilesFailClosed(t *testing.T) {
	for _, source := range []string{"remote", "file", "drop-in"} {
		faults := []string{"malformed", "oversize", "symlink", "directory", "world-writable", "foreign-owner"}
		if source != "remote" {
			faults = append(faults, "group-writable")
		}
		for _, fault := range faults {
			t.Run(source+"/"+fault, func(t *testing.T) {
				home, sources := claudePolicyFixture(t)
				path := filepath.Join(sources.ManagedRoot, "managed-settings.json")
				switch source {
				case "remote":
					path = filepath.Join(home, ".claude", "remote-settings.json")
				case "drop-in":
					path = filepath.Join(sources.ManagedRoot, "managed-settings.d", "10-updates.json")
				}
				data := `{"model":"private-policy-payload","env":{"PRIVATE_TOKEN":"private-token"}}`
				if fault == "malformed" {
					data = `{"model":"private-policy-payload"`
				}
				if fault == "oversize" {
					data = `{"model":"private-policy-payload","padding":"` + strings.Repeat("x", claudeMetadataLimit) + `"}`
				}
				writeClaudePolicyFixture(t, path, data)
				switch fault {
				case "symlink":
					if err := os.Rename(path, path+".target"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(path+".target", path); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				case "world-writable":
					if err := os.Chmod(path, 0666); err != nil {
						t.Fatal(err)
					}
				case "group-writable":
					if err := os.Chmod(path, 0660); err != nil {
						t.Fatal(err)
					}
				case "foreign-owner":
					if os.Geteuid() != 0 {
						t.Skip("changing fixture ownership requires root")
					}
					if err := os.Chown(path, 65534, 65534); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := readClaudeUpdatePolicyWithSources(home, sources); err == nil || strings.Contains(err.Error(), "private-policy-payload") || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), path) {
					t.Fatalf("unsafe managed policy error = %v", err)
				}
			})
		}
	}
}

func TestClaudeUpdatePolicyRejectsUnsafePolicyDirectories(t *testing.T) {
	for _, fault := range []string{"settings-not-directory", "managed-root-not-directory", "drop-ins-not-directory", "managed-root-symlink", "drop-ins-symlink"} {
		t.Run(fault, func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			path := filepath.Join(home, ".claude")
			switch fault {
			case "managed-root-not-directory", "managed-root-symlink":
				path = sources.ManagedRoot
			case "drop-ins-not-directory", "drop-ins-symlink":
				path = filepath.Join(sources.ManagedRoot, "managed-settings.d")
			}
			if strings.HasSuffix(fault, "symlink") {
				if fault == "managed-root-symlink" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			} else {
				if fault == "managed-root-not-directory" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(path, []byte("private-policy-payload"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readClaudeUpdatePolicyWithSources(home, sources); err == nil || strings.Contains(err.Error(), "private-policy-payload") || strings.Contains(err.Error(), path) {
				t.Fatalf("unsafe policy directory error = %v", err)
			}
		})
	}
}

func TestClaudeUpdatePolicyRejectsInvalidManagedUpdateControls(t *testing.T) {
	for _, policy := range []string{
		`{"autoUpdatesChannel":"private-channel"}`,
		`{"autoUpdatesChannel":false}`,
		`{"model":"managed-model","autoUpdatesChannel":null}`,
		`{"model":"managed-model","minimumVersion":null}`,
		`{"model":"managed-model","requiredMinimumVersion":null}`,
		`{"model":"managed-model","requiredMaximumVersion":null}`,
		`{"minimumVersion":"2.1.312-beta.1"}`,
		`{"requiredMinimumVersion":"v2.1.312"}`,
		`{"requiredMaximumVersion":"2.1.312+private-build"}`,
		`{"requiredMaximumVersion":312}`,
		`{"managedSourcesBehavior":"private-unknown-behavior"}`,
		`{"managedSourcesBehavior":true}`,
		`{"env":{"DISABLE_UPDATES":true}}`,
		`{"env":{"CLAUDE_CODE_UPDATE_CHANNEL":"private-channel"}}`,
		`{"DISABLE_UPDATES":"1"}`,
		`{"pinnedVersion":"private-pin"}`,
		`{"settings":{"env":{"DISABLE_UPDATES":"1"}}}`,
	} {
		t.Run(policy, func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			path := filepath.Join(home, ".claude", "remote-settings.json")
			writeClaudePolicyFixture(t, path, policy)
			before := snapshotClaudePolicyFiles(t, path)
			if _, err := readClaudeUpdatePolicyWithSources(home, sources); err == nil || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), path) {
				t.Fatalf("invalid managed update control error = %v", err)
			}
			assertClaudePolicyFilesUnchanged(t, before)
		})
	}
}

func TestClaudeUpdatePolicyRejectsAmbiguousManagedJSON(t *testing.T) {
	for _, data := range []string{
		`null`,
		`[]`,
		`"private-payload"`,
		`{} {"env":{"DISABLE_UPDATES":"1"}}`,
		`{"env":{"DISABLE_UPDATES":"1","DISABLE_UPDATES":"0"}}`,
		strings.Repeat(`{"private-payload":`, 66) + `null` + strings.Repeat(`}`, 66),
	} {
		t.Run(data[:min(len(data), 70)], func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), data)
			if _, err := readClaudeUpdatePolicyWithSources(home, sources); err == nil || strings.Contains(err.Error(), "private-payload") {
				t.Fatalf("ambiguous managed JSON error = %v", err)
			}
		})
	}
}

func TestClaudeUpdatePolicyHelperRespectsSelectedManagedSource(t *testing.T) {
	for _, tc := range []struct {
		name, remote, managed string
		wantError             bool
	}{
		{
			name:      "selected-file-helper-requires-manual-update",
			managed:   `{"policyHelper":{"path":"/private/must-not-execute","timeoutMs":5000}}`,
			wantError: true,
		},
		{
			name:    "shadowed-file-helper-is-inactive",
			remote:  `{"model":"managed-model"}`,
			managed: `{"policyHelper":{"path":"/private/must-not-execute","timeoutMs":5000}}`,
		},
		{
			name:   "remote-helper-is-not-an-OS-policy-source",
			remote: `{"policyHelper":{"path":"/private/must-not-execute","timeoutMs":5000}}`,
		},
		{
			name:    "merge-does-not-activate-shadowed-helper",
			remote:  `{"managedSourcesBehavior":"merge","model":"managed-model"}`,
			managed: `{"policyHelper":{"path":"/private/must-not-execute","timeoutMs":5000}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			for path, data := range map[string]string{
				filepath.Join(home, ".claude", "remote-settings.json"):      tc.remote,
				filepath.Join(sources.ManagedRoot, "managed-settings.json"): tc.managed,
			} {
				if data != "" {
					writeClaudePolicyFixture(t, path, data)
				}
			}
			if _, err := readClaudeUpdatePolicyWithSources(home, sources); (err != nil) != tc.wantError || (err != nil && strings.Contains(err.Error(), "/private/")) {
				t.Fatalf("helper selection error = %v; want error %v", err, tc.wantError)
			}
		})
	}
}

func TestClaudeUpdatePolicyUninspectableMDMRequiresManualUpdate(t *testing.T) {
	home, sources := claudePolicyFixture(t)
	path := filepath.Join(sources.ManagedRoot, "managed-preferences.plist")
	sources.MDMPaths = []string{path}
	if _, err := readClaudeUpdatePolicyWithSources(home, sources); err != nil {
		t.Fatalf("absent MDM policy blocked updates: %v", err)
	}
	writeClaudePolicyFixture(t, filepath.Join(home, ".claude", "remote-settings.json"), `{"model":"managed-model"}`)
	writeClaudePolicyFixture(t, path, "private-mdm-policy-payload")
	before := snapshotClaudePolicyFiles(t, path)
	if _, err := readClaudeUpdatePolicyWithSources(home, sources); err == nil || strings.Contains(err.Error(), "private-mdm-policy-payload") || strings.Contains(err.Error(), path) {
		t.Fatalf("uninspectable MDM error = %v", err)
	}
	assertClaudePolicyFilesUnchanged(t, before)
}

func TestClaudeUpdatePolicyManagedMCPIsNotUpdatePolicy(t *testing.T) {
	for _, kind := range []string{"malformed", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			home, sources := claudePolicyFixture(t)
			path := filepath.Join(sources.ManagedRoot, "managed-mcp.json")
			writeClaudePolicyFixture(t, path, "private-mcp-payload that is not JSON")
			if kind == "symlink" {
				if err := os.Rename(path, path+".target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".target", path); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotClaudePolicyFiles(t, path)
			policy, err := readClaudeUpdatePolicyWithSources(home, sources)
			if err != nil || policy != (claudeUpdatePolicy{Channel: "latest"}) {
				t.Fatalf("MCP file was treated as runtime update policy: %+v, %v", policy, err)
			}
			assertClaudePolicyFilesUnchanged(t, before)
		})
	}
}
