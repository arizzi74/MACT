package releasemanager

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type claudeUpdatePolicy struct {
	Channel, MinimumVersion, MaximumVersion string
}

// Only tests can replace these paths. Production has no environment or command
// line escape hatch around the native machine policy locations.
type claudeUpdatePolicySources struct {
	ManagedRoot string
	MDMPaths    []string
}

func nativeClaudeUpdatePolicySources(home string) claudeUpdatePolicySources {
	sources := claudeUpdatePolicySources{ManagedRoot: "/etc/claude-code"}
	if runtime.GOOS == "darwin" {
		sources.ManagedRoot = "/Library/Application Support/ClaudeCode"
		sources.MDMPaths = []string{
			"/Library/Managed Preferences/com.anthropic.claudecode.plist",
			filepath.Join("/Library/Managed Preferences", filepath.Base(home), "com.anthropic.claudecode.plist"),
		}
	}
	return sources
}

func (m *Manager) readClaudeUpdatePolicy(home string) (claudeUpdatePolicy, error) {
	if m.claudePolicySources != nil {
		return readClaudeUpdatePolicyWithSources(home, *m.claudePolicySources)
	}
	return readClaudeUpdatePolicy(home)
}

func readClaudeUpdatePolicy(home string) (claudeUpdatePolicy, error) {
	return readClaudeUpdatePolicyWithSources(home, nativeClaudeUpdatePolicySources(home))
}

// Read only policy, never account configuration or credentials. This evaluates
// the last native remote-policy cache. The native CLI still owns fetching and
// enforcing fresh policy at its next launch.
// forceRemoteSettingsRefresh controls that startup fetch, not runtime updates.
// See https://code.claude.com/docs/en/managed-settings#how-claude-code-combines-managed-sources
// and https://code.claude.com/docs/en/settings-reference#updates-and-versioning.
func readClaudeUpdatePolicyWithSources(home string, sources claudeUpdatePolicySources) (claudeUpdatePolicy, error) {
	policy := claudeUpdatePolicy{Channel: "latest"}
	if !filepath.IsAbs(home) || !filepath.IsAbs(sources.ManagedRoot) {
		return policy, errors.New("cannot inspect Claude runtime update policy without absolute configuration paths")
	}
	home = filepath.Clean(home)
	settingsDir := filepath.Join(home, ".claude")
	if config := os.Getenv("CLAUDE_CONFIG_DIR"); config != "" && filepath.Clean(config) != settingsDir {
		return policy, errors.New("custom Claude configuration directories require manual runtime updates")
	}
	if err := claudePolicyDirectory(settingsDir, false); err != nil {
		return policy, err
	}
	user, err := readClaudePolicyDocument(filepath.Join(settingsDir, "settings.json"), false)
	if err != nil {
		return policy, err
	}
	remote, err := readClaudePolicyDocument(filepath.Join(settingsDir, "remote-settings.json"), false)
	if err != nil {
		return policy, err
	}
	if len(remote) != 0 {
		if err := validateClaudeRemotePolicyRouting(user); err != nil {
			return policy, err
		}
	}
	// A plist can deliver cross-source env restrictions even when remote policy
	// wins. Until native MDM evaluation is supported, its presence remains closed.
	for _, path := range sources.MDMPaths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return policy, errors.New("cannot safely evaluate Claude runtime update policy supplied by MDM")
		}
	}
	filePolicy, err := readClaudeMachinePolicy(sources.ManagedRoot)
	if err != nil {
		return policy, err
	}
	managedSources := []map[string]any{remote, filePolicy}
	mergeManaged, selected := false, -1
	for _, source := range managedSources {
		behavior, hasBehavior := source["managedSourcesBehavior"]
		if !hasBehavior || behavior == nil {
			if !claudeHasPolicyKeys(source) {
				continue
			}
			break
		}
		value, ok := behavior.(string)
		if !ok || (value != "first-wins" && value != "merge") {
			return policy, errors.New("unsupported Claude managed policy precedence")
		}
		mergeManaged = value == "merge"
		break
	}
	for index, source := range managedSources {
		if claudeHasPolicyKeys(source) {
			selected = index
			break
		}
	}
	// A helper in the selected machine source replaces every static source.
	// Running a policy program here would expand this updater's authority.
	if selected == 1 && filePolicy["policyHelper"] != nil {
		return policy, errors.New("computed Claude policy requires administrator-controlled runtime updates")
	}
	effectiveManaged := make(map[string]any)
	for index := len(managedSources) - 1; index >= 0; index-- {
		if mergeManaged || index == selected {
			if err := validateClaudeUpdatePolicyKeys(managedSources[index]); err != nil {
				return policy, err
			}
			mergeClaudePolicy(effectiveManaged, managedSources[index])
		}
	}
	if err := validateClaudeUpdatePolicyKeys(user); err != nil {
		return policy, err
	}
	effective := make(map[string]any)
	mergeClaudePolicy(effective, user)
	// These keys have managed scope. Native Claude ignores copies in user files.
	delete(effective, "requiredMinimumVersion")
	delete(effective, "requiredMaximumVersion")
	mergeClaudePolicy(effective, effectiveManaged)
	// env is an explicit exception to whole-source managed precedence: each
	// variable comes from the highest admin source that sets that variable.
	env := make(map[string]any)
	for _, source := range []map[string]any{user, filePolicy, remote} {
		value, present := source["env"]
		if !present || value == nil {
			continue
		}
		block, ok := value.(map[string]any)
		if !ok {
			return policy, errors.New("invalid Claude runtime update environment policy")
		}
		for key, value := range block {
			if claudeUnknownUpdateEnvironmentKey(key) {
				return policy, errors.New("unsupported Claude runtime update environment policy")
			}
			env[key] = value
		}
	}
	// For standalone Claude, settings.env overrides inherited shell exports.
	// Autonomous updates respect the background-updater opt-out as well as the
	// stronger prohibition on all update paths.
	for _, key := range []string{"DISABLE_UPDATES", "DISABLE_AUTOUPDATER", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"} {
		value := os.Getenv(key)
		if setting, exists := env[key]; exists {
			var ok bool
			value, ok = setting.(string)
			if !ok {
				return policy, errors.New("invalid Claude runtime update environment policy")
			}
		}
		if key == "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" {
			if value != "" {
				return policy, errors.New("Claude runtime updates are disabled by native settings")
			}
			continue
		}
		switch strings.ToLower(value) {
		case "", "0", "false", "no", "off":
		case "1", "true", "yes", "on":
			return policy, errors.New("Claude runtime updates are disabled by native settings")
		default:
			return policy, errors.New("invalid Claude runtime update environment policy")
		}
	}
	channel, err := claudePolicyString(effective, "autoUpdatesChannel")
	if err != nil {
		return policy, err
	}
	if channel != "" {
		if channel != "latest" && channel != "stable" {
			return policy, errors.New("unsupported Claude runtime update channel")
		}
		policy.Channel = channel
	}
	minimum, err := claudePolicyString(effective, "minimumVersion")
	if err != nil {
		return policy, err
	}
	requiredMinimum, err := claudePolicyString(effective, "requiredMinimumVersion")
	if err != nil {
		return policy, err
	}
	maximum, err := claudePolicyString(effective, "requiredMaximumVersion")
	if err != nil {
		return policy, err
	}
	for _, version := range []string{minimum, requiredMinimum, maximum} {
		if version != "" {
			if parsed, err := parseClaudeVersion(version); err != nil || parsed != version {
				return policy, errors.New("invalid Claude runtime version policy")
			}
		}
	}
	policy.MinimumVersion, policy.MaximumVersion = minimum, maximum
	if requiredMinimum != "" {
		if comparison, _ := CompareVersions(requiredMinimum, minimum); minimum == "" || comparison > 0 {
			policy.MinimumVersion = requiredMinimum
		}
	}
	if policy.MinimumVersion != "" && maximum != "" {
		if comparison, _ := CompareVersions(policy.MinimumVersion, maximum); comparison > 0 {
			return policy, errors.New("conflicting Claude runtime version policy")
		}
	}
	return policy, nil
}

func claudePolicyString(source map[string]any, key string) (string, error) {
	value, present := source[key]
	if !present {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("invalid Claude runtime update settings")
	}
	return text, nil
}

// Remote delivery eligibility is checked before managed env is applied. A cache
// plus explicitly different routing cannot prove whether remote or local policy
// supplies the active version bounds, including on gateway deployments. Keep
// those installations closed instead of letting a stale cache shadow a pin.
// https://code.claude.com/docs/en/server-managed-settings#platform-availability
func validateClaudeRemotePolicyRouting(user map[string]any) error {
	env := map[string]any{}
	if value := user["env"]; value != nil {
		var ok bool
		env, ok = value.(map[string]any)
		if !ok {
			return errors.New("invalid Claude runtime update environment policy")
		}
	}
	valueFor := func(key string) (string, error) {
		if value, exists := env[key]; exists {
			text, ok := value.(string)
			if !ok {
				return "", errors.New("invalid Claude cached policy provider routing")
			}
			return text, nil
		}
		return os.Getenv(key), nil
	}
	for _, key := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS"} {
		value, err := valueFor(key)
		if err != nil {
			return err
		}
		switch strings.ToLower(value) {
		case "", "0", "false", "no", "off":
		case "1", "true", "yes", "on":
			return errors.New("cannot safely evaluate cached Claude policy with custom provider routing")
		default:
			return errors.New("invalid Claude cached policy provider routing")
		}
	}
	baseURL, err := valueFor("ANTHROPIC_BASE_URL")
	if err != nil || baseURL == "" {
		return err
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Scheme != "https" || !strings.EqualFold(endpoint.Hostname(), "api.anthropic.com") || (endpoint.Port() != "" && endpoint.Port() != "443") || endpoint.User != nil || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("cannot safely evaluate cached Claude policy with custom provider routing")
	}
	return nil
}

func claudeHasPolicyKeys(source map[string]any) bool {
	for key, value := range source {
		if value != nil && key != "managedSourcesBehavior" && key != "wslInheritsWindowsSettings" {
			return true
		}
	}
	return false
}

// Unknown unrelated settings are deliberately harmless. An unrecognized update
// control or wrapper could conceal a restriction, so it cannot be approximated.
func claudeUnknownUpdatePolicyKey(key string) bool {
	switch key {
	case "autoUpdatesChannel", "minimumVersion", "requiredMinimumVersion", "requiredMaximumVersion", "DISABLE_UPDATES", "DISABLE_AUTOUPDATER", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC":
		return false
	}
	key = strings.ToLower(key)
	return strings.Contains(key, "updat") || strings.Contains(key, "version") || strings.HasPrefix(key, "pinned") || key == "pin"
}

func claudeUnknownUpdateEnvironmentKey(key string) bool {
	upper := strings.ToUpper(key)
	if !strings.HasPrefix(upper, "CLAUDE_") && !strings.HasPrefix(upper, "ANTHROPIC_") && !strings.HasPrefix(upper, "DISABLE_") {
		return false
	}
	return claudeUnknownUpdatePolicyKey(key)
}

func validateClaudeUpdatePolicyKeys(source map[string]any) error {
	for key, value := range source {
		if value == nil {
			continue
		}
		if key == "settings" || key == "managedSettings" || key == "policy" || key == "DISABLE_UPDATES" || key == "DISABLE_AUTOUPDATER" || key == "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" || claudeUnknownUpdatePolicyKey(key) {
			return errors.New("unsupported Claude runtime update policy settings")
		}
	}
	return nil
}

func mergeClaudePolicy(destination, source map[string]any) {
	for key, value := range source {
		if block, ok := value.(map[string]any); ok {
			merged, _ := destination[key].(map[string]any)
			if merged == nil {
				merged = make(map[string]any)
			}
			mergeClaudePolicy(merged, block)
			destination[key] = merged
		} else {
			destination[key] = value
		}
	}
}

func readClaudeMachinePolicy(root string) (map[string]any, error) {
	if err := claudePolicyDirectory(root, true); err != nil {
		return nil, err
	}
	settings, err := readClaudePolicyDocument(filepath.Join(root, "managed-settings.json"), true)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		settings = make(map[string]any)
	}
	dropins := filepath.Join(root, "managed-settings.d")
	if err := claudePolicyDirectory(dropins, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dropins)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return nil, errors.New("cannot read Claude managed runtime update policy")
	}
	// os.ReadDir sorts names lexically. Native policy ignores dotfiles and any
	// filename without a .json suffix. managed-mcp.json is a separate MCP policy.
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fragment, err := readClaudePolicyDocument(filepath.Join(dropins, entry.Name()), true)
		if err != nil {
			return nil, err
		}
		mergeClaudePolicy(settings, fragment)
	}
	return settings, nil
}

func claudePolicyDirectory(path string, managed bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !claudePolicyFileSafe(info, true, managed) {
		return errors.New("cannot safely inspect Claude runtime update policy directory")
	}
	return nil
}

func claudePolicyFileSafe(info os.FileInfo, directory, managed bool) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) || info.Mode().Perm()&0002 != 0 {
		return false
	}
	if managed && info.Mode().Perm()&0020 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (int(stat.Uid) == os.Geteuid() || (managed && stat.Uid == 0))
}

func readClaudePolicyDocument(path string, managed bool) (map[string]any, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !claudePolicyFileSafe(info, false, managed) {
		return nil, errors.New("cannot safely inspect Claude runtime update policy file")
	}
	// NOFOLLOW also protects the final component if it changes after Lstat;
	// NONBLOCK prevents a raced FIFO from blocking the updater.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot read Claude runtime update policy file")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !claudePolicyFileSafe(opened, false, managed) {
		return nil, errors.New("Claude runtime update policy file changed while reading")
	}
	data, err := io.ReadAll(io.LimitReader(file, claudeMetadataLimit+1))
	if err != nil || len(data) > claudeMetadataLimit {
		return nil, errors.New("cannot read Claude runtime update policy within its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeClaudePolicyJSON(decoder, 0)
	var trailing any
	if err != nil || decoder.Decode(&trailing) != io.EOF {
		return nil, errors.New("invalid Claude runtime update policy JSON")
	}
	document, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid Claude runtime update policy JSON object")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("Claude runtime update policy file changed while reading")
	}
	return document, nil
}

// Reject duplicate keys instead of guessing which interpretation native schema
// validation would keep. Limit nesting independently of the document size limit.
func decodeClaudePolicyJSON(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("policy nesting limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid policy object key")
			}
			if _, exists := object[key]; exists {
				return nil, errors.New("duplicate policy object key")
			}
			value, err := decodeClaudePolicyJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, errors.New("invalid policy object")
		}
		return object, nil
	case json.Delim('['):
		var array []any
		for decoder.More() {
			value, err := decodeClaudePolicyJSON(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, errors.New("invalid policy array")
		}
		return array, nil
	default:
		return token, nil
	}
}
