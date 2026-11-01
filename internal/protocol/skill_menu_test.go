package protocol

import "testing"

func TestSkillMenuRejectsUntrustedNamesAndControlArguments(t *testing.T) {
	for _, name := range []string{"", "../x", "/clear", "bad\nclear", "--page", "x y", "x;sh"} {
		if ValidSkillName(name) {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, name := range []string{"review", "plugin:review", "árbol", "foo.bar", "x_y"} {
		if !ValidSkillName(name) {
			t.Fatalf("rejected %q", name)
		}
	}
	for _, args := range []string{"--page -1", "--page 01", "--page 1\n", "--page 1001"} {
		if _, ok := SkillPage(args); ok {
			t.Fatalf("accepted invalid page %q", args)
		}
	}
	m := &SkillMenu{Options: []SkillOption{{Args: "review", Label: "Review"}, {Args: "--page 1", Label: "Next"}}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.Options = append(m.Options, m.Options[0])
	if m.Validate() == nil {
		t.Fatal("duplicate skill option accepted")
	}
}
