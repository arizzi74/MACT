package claudeadapter

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// childEnvironment is shared by native launch and legacy deletion discovery.
// It never evaluates a settings script or consults credentials for path data.
func childEnvironment(base []string, overrides map[string]string) ([]string, error) {
	if base == nil {
		base = os.Environ()
	}
	env := append([]string(nil), base...)
	keys := make([]string, 0, len(overrides))
	for key, value := range overrides {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, 0) {
			return nil, errors.New("Invalid Claude environment setting")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		filtered := env[:0]
		for _, entry := range env {
			if !strings.HasPrefix(entry, key+"=") {
				filtered = append(filtered, entry)
			}
		}
		env = append(filtered, key+"="+overrides[key])
	}
	return env, nil
}

func environmentValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, key+"=") {
			value = strings.TrimPrefix(entry, key+"=")
		}
	}
	return value
}

// Claude's official session reader uses CLAUDE_CONFIG_DIR/projects, falling
// back to the process user's home/.claude/projects. Resolve existing symlinks
// once at launch and retain the physical root with the adapter-owned session.
// https://github.com/anthropics/claude-agent-sdk-python/blob/main/src/claude_agent_sdk/_internal/sessions.py
func nativeConfigDirectory(env []string, cwd string) (string, error) {
	path := environmentValue(env, "CLAUDE_CONFIG_DIR")
	if path == "" {
		home := environmentValue(env, "HOME")
		if runtime.GOOS == "windows" {
			home = environmentValue(env, "USERPROFILE")
		}
		if home == "" {
			account, err := user.Current()
			if err != nil || account.HomeDir == "" {
				return "", errors.New("Claude native home directory is unavailable")
			}
			home = account.HomeDir
		}
		path = filepath.Join(home, ".claude")
	}
	path = norm.NFC.String(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return "", errors.New("Claude native configuration directory is unsafe")
	}
	var suffix []string
	ancestor := path
	for {
		physical, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				physical = filepath.Join(physical, suffix[i])
			}
			return filepath.Clean(physical), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("Claude native configuration directory is unavailable")
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", errors.New("Claude native configuration directory is unavailable")
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
	}
}

func (s *server) rememberNativeRoot(t *savedThread, env []string) error {
	root, err := nativeConfigDirectory(env, t.CWD)
	if err != nil {
		return err
	}
	for _, existing := range t.NativeConfigRoots {
		if existing == root {
			return nil
		}
	}
	if len(t.NativeConfigRoots) >= 32 {
		return errors.New("Claude session has too many native configuration directories")
	}
	t.NativeConfigRoots = append(t.NativeConfigRoots, root)
	return s.save(t)
}
