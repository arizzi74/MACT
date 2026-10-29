package claudeadapter

import (
	"strings"
	"testing"
)

func TestCheckUpdateIdleRejectsNativeWork(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*server)
		want   string
	}{
		{"closed", func(s *server) { s.closed = true }, "closed"},
		{"deletion", func(s *server) { s.deletions["deleted"] = &deletionRecord{} }, "incomplete session deletion"},
		{"approval or question", func(s *server) { s.pending["permission"] = &permission{} }, "pending approvals or questions"},
		{"active turn", func(s *server) { s.threads["thread"].Turns = []*savedTurn{{Status: "inProgress"}} }, "active turn"},
		{"unloaded queued turn", func(s *server) { s.threads["thread"].Queue = []*queuedInput{{ID: "queued"}} }, "active turn"},
		{"background task", func(s *server) {
			s.runners["thread"] = &process{tasks: map[string]*backgroundTask{"agent": {active: true, background: true}}}
		}, "background work"},
		{"native background snapshot", func(s *server) {
			s.runners["thread"] = &process{backgroundSet: map[string]string{"shell": "run"}}
		}, "background work"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &server{threads: map[string]*savedThread{"thread": {ID: "thread"}}, deletions: map[string]*deletionRecord{}, pending: map[string]*permission{}, runners: map[string]*process{}}
			test.change(s)
			r := &Runtime{server: s}
			err := r.CheckUpdateIdle()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unsafe native work accepted: %v", err)
			}
			called := false
			settingsErr := r.WithIdleSettingsUpdate(func() error { called = true; return nil })
			if settingsErr == nil || called || settingsErr.Error() != err.Error() {
				t.Fatalf("settings and update idle checks differ: update=%v settings=%v called=%t", err, settingsErr, called)
			}
		})
	}
	var missing *Runtime
	if err := missing.CheckUpdateIdle(); err == nil {
		t.Fatal("missing runtime accepted")
	}
}

func TestCheckUpdateIdlePreservesIdleProcessesAndPrivateState(t *testing.T) {
	thread := &savedThread{ID: "thread", Turns: []*savedTurn{{Status: "completed"}}}
	native := &process{done: make(chan struct{}), tasks: map[string]*backgroundTask{"completed": {active: false, background: true}}}
	balance := &CreditBalance{Remaining: 12, Unit: "USD"}
	s := &server{threads: map[string]*savedThread{thread.ID: thread}, runners: map[string]*process{thread.ID: native}, deletions: map[string]*deletionRecord{"deleted": {Done: true}}, balance: balance, balanceGeneration: 7}
	s.cfg.LoadSettings = func() (LaunchSettings, error) {
		t.Fatal("idle observation read private settings")
		return LaunchSettings{}, nil
	}
	r := &Runtime{server: s}
	for range 2 {
		if err := r.CheckUpdateIdle(); err != nil {
			t.Fatal(err)
		}
	}
	if s.runners[thread.ID] != native || s.threads[thread.ID] != thread || s.balance != balance || s.balanceGeneration != 7 {
		t.Fatal("idle observation changed native process, history, balance, or settings generation")
	}
	select {
	case <-native.done:
		t.Fatal("idle observation stopped the native process")
	default:
	}
}
