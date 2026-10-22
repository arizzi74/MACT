package releasemanager

import (
	"context"
	"encoding/json"
	"errors"
)

// These identities stay fixed independently of DefaultRepo. Only this one
// repository rename is trusted; historical releases keep their original signer.
const LegacyRepo = "arizzi74/Codex-Telegram-Gateway"
const RenamedRepo = "arizzi74/MACT"
const officialRepositoryID int64 = 1369450599
const minimumRepositoryBridgeTag = "v0.6.21"

func validateRepositoryBridgeInstall(release *Release) error {
	if release.Repo != RenamedRepo {
		return nil
	}
	comparison, err := CompareVersions(release.Tag, minimumRepositoryBridgeTag)
	if err != nil {
		return err
	}
	if comparison < 0 {
		return errors.New("releases before v0.6.21 contain an updater that cannot follow the MACT repository rename; install v0.6.21 or newer")
	}
	return nil
}

func (m *Manager) releaseRepository(ctx context.Context, requested, tag, htmlURL string) (string, error) {
	if htmlURL == "" || htmlURL == "https://github.com/"+requested+"/releases/tag/"+tag {
		return requested, nil
	}
	if requested == LegacyRepo && htmlURL == "https://github.com/"+RenamedRepo+"/releases/tag/"+tag {
		data, err := m.Download(ctx, "https://api.github.com/repos/"+LegacyRepo, 1024*1024)
		if err != nil {
			return "", err
		}
		var repository struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		}
		if err := json.Unmarshal(data, &repository); err != nil || repository.ID != officialRepositoryID || repository.FullName != RenamedRepo {
			return "", errors.New("GitHub did not confirm the authorized repository rename and immutable repository identity")
		}
		return RenamedRepo, nil
	}
	return "", errors.New("release URL does not match the selected repository and tag or its authorized rename")
}

// Keep the previous official repository while readiness is pending. A failed
// start must not commit the rename; the next successful update can resolve it
// again. Fork selections keep their independent repository policy.
func pendingReleaseRepository(l *Layout, release *Release) (string, error) {
	if release.Repo != RenamedRepo {
		return release.Repo, nil
	}
	if !FileExists(l.State) {
		if release.RequestedRepo != "" {
			return release.RequestedRepo, nil
		}
		return release.Repo, nil
	}
	settings, err := SavedSettings(l)
	if err != nil {
		return "", err
	}
	if repo, _ := settings["repo"].(string); repo == LegacyRepo {
		return repo, nil
	}
	return release.Repo, nil
}
