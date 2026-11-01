package protocol

import (
	"errors"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const SkillMenuPageSize = 8

type Skill struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ArgumentHint string `json:"argument_hint,omitempty"`
}

type SkillMenu struct {
	Options []SkillOption `json:"options"`
	// Entries support local browser filtering without repeated native probes.
	Entries []Skill `json:"entries,omitempty"`
}

type SkillOption struct {
	Args        string `json:"args"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func ValidSkillName(name string) bool {
	if name == "" || len(name) > 256 || !utf8.ValidString(name) || strings.HasPrefix(name, "-") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_:.", r) {
			return false
		}
	}
	return true
}

func SkillPage(args string) (int, bool) {
	if args == "" {
		return 0, true
	}
	if !strings.HasPrefix(args, "--page ") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(args, "--page "))
	return n, err == nil && n >= 0 && n <= 1000 && args == "--page "+strconv.Itoa(n)
}

func (m *SkillMenu) Validate() error {
	if m == nil || len(m.Options) == 0 || len(m.Options) > SkillMenuPageSize+2 || len(m.Entries) > 1000 {
		return errors.New("invalid skill menu size")
	}
	seen := map[string]bool{}
	for _, o := range m.Options {
		_, page := SkillPage(o.Args)
		if (!page && !ValidSkillName(o.Args)) || o.Args == "" || seen[o.Args] || !validSkillText(o.Label, 120) || o.Label == "" || !validSkillText(o.Description, 500) {
			return errors.New("invalid skill menu option")
		}
		seen[o.Args] = true
	}
	seen = map[string]bool{}
	for _, e := range m.Entries {
		if !ValidSkillName(e.Name) || seen[e.Name] || !validSkillText(e.Description, 500) || !validSkillText(e.ArgumentHint, 256) {
			return errors.New("invalid skill metadata")
		}
		seen[e.Name] = true
	}
	return nil
}

func validSkillText(s string, limit int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= limit && strings.IndexFunc(s, unicode.IsControl) < 0
}
