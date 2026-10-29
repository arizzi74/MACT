package releasemanager

import (
	"testing"

	"github.com/iaia/telegramgw/internal/config"
)

func TestWorkerRecoveryConfigComparisonIgnoresOnlyLocalSourceFilename(t *testing.T) {
	base := config.WorkerConfig{WorkerID: "worker", Name: "worker", StateFile: "/state/worker.db", TokenFile: "/private/token", GatewayURL: "wss://gateway.example.test", Runtimes: []config.RuntimeProfile{{ID: "primary", WorkingDirectory: "/project"}}, AllowedWorkspaceRoots: []string{"/project"}}
	other := base
	other.ConfigFile = "/staged/worker.json"
	if !sameWorkerConfiguration(base, other) {
		t.Fatal("source metadata changed enrollment identity")
	}
	for _, mutate := range []func(*config.WorkerConfig){
		func(c *config.WorkerConfig) { c.WorkerID = "different" },
		func(c *config.WorkerConfig) { c.TokenFile = "/other/token" },
		func(c *config.WorkerConfig) { c.StateFile = "/other/state" },
		func(c *config.WorkerConfig) { c.GatewayURL = "wss://other.example.test" },
		func(c *config.WorkerConfig) { c.AllowedWorkspaceRoots = []string{"/"} },
		func(c *config.WorkerConfig) { c.Runtimes = []config.RuntimeProfile{{ID: "other"}} },
		func(c *config.WorkerConfig) { c.RedactPatterns = []string{"different"} },
	} {
		other = base
		mutate(&other)
		if sameWorkerConfiguration(base, other) {
			t.Fatal("changed persisted enrollment setting was ignored")
		}
	}
	if base.ConfigFile != "" {
		t.Fatal("comparison mutated source configuration")
	}
}
