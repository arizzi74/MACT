package codexadapter

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNewThreadDefaultsApplyInSingleStartRPC(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() {
		_, err := client.StartThread(context.Background(), ThreadOptions{
			Model: "worker-model", ReasoningEffort: "high", CWD: "/work", HistoryMode: "legacy",
			ApprovalPolicy: "on-request", ApprovalsReviewer: "user", PermissionProfile: ":read-only",
		})
		done <- err
	}()
	request := fake.next(t)
	p := params(t, request)
	var config map[string]string
	if err := json.Unmarshal(p["config"], &config); err != nil || method(t, request) != "thread/start" || string(p["model"]) != `"worker-model"` || config["model_reasoning_effort"] != "high" || string(p["permissions"]) != `":read-only"` || string(p["approvalsReviewer"]) != `"user"` || string(p["approvalPolicy"]) != `"on-request"` || p["sandbox"] != nil || string(p["historyMode"]) != `"legacy"` {
		t.Fatalf("atomic new thread defaults: %s, %v", request["params"], err)
	}
	fake.respond(t, request, map[string]any{"thread": map[string]any{"id": "fresh", "cwd": "/work"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := client.StartThread(context.Background(), ThreadOptions{PermissionProfile: ":workspace", Sandbox: "read-only"}); err == nil {
		t.Fatal("conflicting native permission strategies were sent")
	}
}

func TestThreadResumeDoesNotApplyNewThreadEffort(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan error, 1)
	go func() {
		_, err := client.ResumeThread(context.Background(), "existing", ThreadOptions{ReasoningEffort: "high", HistoryMode: "legacy"})
		done <- err
	}()
	request := fake.next(t)
	p := params(t, request)
	if method(t, request) != "thread/resume" || p["config"] != nil || p["effort"] != nil || p["historyMode"] != nil || len(p) != 2 {
		t.Fatalf("resume inherited new-session defaults: %s", request["params"])
	}
	fake.respond(t, request, map[string]any{"thread": map[string]any{"id": "existing"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
