package worker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func claudeSkillInvocation(name, args string) bool {
	_, menu := protocol.SkillPage(strings.TrimSpace(args))
	return (name == "skill" || name == "skills") && !menu
}

func (s *sessionActor) claudeSkills(ctx context.Context, client *codexadapter.Client, args string) (protocol.Result, error) {
	if page, menu := protocol.SkillPage(args); menu {
		entries, err := client.ListClaudeSkills(ctx, s.session.ThreadID)
		if err != nil {
			return protocol.Result{}, err
		}
		for i := range entries {
			entries[i].Description = permissionText(s.agent.redactor.Redact(entries[i].Description), 500)
			entries[i].ArgumentHint = permissionText(s.agent.redactor.Redact(entries[i].ArgumentHint), 256)
		}
		if len(entries) == 0 {
			return protocol.Result{Text: "No user-invocable skills are available in this Claude session."}, nil
		}
		pages := (len(entries) + protocol.SkillMenuPageSize - 1) / protocol.SkillMenuPageSize
		if page >= pages {
			return protocol.Result{}, validationError("Skill page is unavailable. Run /skill to refresh the list.")
		}
		menu := &protocol.SkillMenu{Entries: entries}
		for _, e := range entries[page*protocol.SkillMenuPageSize : min((page+1)*protocol.SkillMenuPageSize, len(entries))] {
			menu.Options = append(menu.Options, protocol.SkillOption{Args: e.Name, Label: permissionText(e.Name, 120), Description: e.Description})
		}
		if page > 0 {
			menu.Options = append(menu.Options, protocol.SkillOption{Args: "--page " + strconv.Itoa(page-1), Label: "Previous skills"})
		}
		if page+1 < pages {
			menu.Options = append(menu.Options, protocol.SkillOption{Args: "--page " + strconv.Itoa(page+1), Label: "Next skills"})
		}
		if err := menu.Validate(); err != nil {
			return protocol.Result{}, validationError("Claude returned invalid skill metadata.")
		}
		return protocol.Result{Text: fmt.Sprintf("Claude skills (%d available, page %d/%d). Choose a skill, or use /skill NAME [arguments].", len(entries), page+1, pages), SkillMenu: menu}, nil
	}
	name, arguments := args, ""
	if at := strings.IndexFunc(args, unicode.IsSpace); at >= 0 {
		name, arguments = args[:at], strings.TrimLeftFunc(args[at:], unicode.IsSpace)
	}
	if !protocol.ValidSkillName(name) {
		return protocol.Result{}, validationError("Use /skill [name] [arguments], or /skill to list available skills.")
	}
	turn, queued, err := client.StartClaudeSkill(ctx, s.session.ThreadID, name, arguments, s.session.ActiveTurnID != "" || s.session.State == "running")
	if err != nil {
		return protocol.Result{}, err
	}
	if queued {
		return protocol.Result{Text: "Skill " + name + " queued after the active turn.", State: "queued"}, nil
	}
	return protocol.Result{Text: "Running skill " + name + ".", State: "running", TurnID: turn.ID}, nil
}
