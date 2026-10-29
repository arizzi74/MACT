package releasemanager

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/iaia/telegramgw/internal/workerupdate"
)

func TestWorkerAbortNeverInvokesCommandWithLeaseToken(t *testing.T) {
	dir, err := os.MkdirTemp("", "mact-abort-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	state := filepath.Join(dir, "worker.db")
	l := &Layout{Config: filepath.Join(dir, "config.json"), Binary: "/must/not/execute"}
	if err := WriteJSON(l.Config, map[string]any{"state_file": state, "worker_id": "worker"}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", workerupdate.ControlPath(state))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			received <- ""
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		var request map[string]string
		_ = json.NewDecoder(connection).Decode(&request)
		_ = json.NewEncoder(connection).Encode(map[string]bool{"aborted": true})
		received <- request["token"]
	}()
	m := New(nil)
	m.Run = func(context.Context, ...string) (CommandResult, error) {
		t.Error("abort spawned subprocess")
		return CommandResult{}, nil
	}
	// Relative state paths resolve just as they do in config.LoadWorker.
	if err := WriteJSON(l.Config, map[string]any{"state_file": "worker.db", "worker_id": "worker"}); err != nil {
		t.Fatal(err)
	}
	if err := m.workerAbort(t.Context(), l, "synthetic-private-lease"); err != nil {
		t.Fatal(err)
	}
	if <-received != "synthetic-private-lease" {
		t.Fatal("private abort failed")
	}
}
