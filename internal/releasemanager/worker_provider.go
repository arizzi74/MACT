package releasemanager

// The durable release transaction remains a worker transaction for either
// provider. Executables, services and local state belong to distinct installs.
func componentBinary(l *Layout) string {
	if l.Component == "worker" && l.AgentKind == "claude" {
		return "claude-worker"
	}
	return "codex-" + l.Component
}

func componentCommand(l *Layout) string {
	if l.Component == "worker" && l.AgentKind == "claude" {
		return "claude-worker"
	}
	return l.Component
}

func workerLabel(l *Layout) string { return "com.iaia." + componentBinary(l) }
