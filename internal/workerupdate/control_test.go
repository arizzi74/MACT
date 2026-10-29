package workerupdate

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAbortLeaseUsesPrivateSocketAndExistingWireProtocol(t *testing.T) {
	for _, reply := range []string{`{"aborted":true}`, `{"aborted":false}`, `{"error":"private server detail"}`, `{broken`} {
		t.Run(reply, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "mact-control-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			state := filepath.Join(dir, "worker.db")
			listener, err := net.Listen("unix", ControlPath(state))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			requests := make(chan map[string]string, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					requests <- nil
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
				var request map[string]string
				_ = json.NewDecoder(connection).Decode(&request)
				_, _ = connection.Write([]byte(reply + "\n"))
				requests <- request
			}()
			token := "synthetic-lease-only"
			err = AbortLease(t.Context(), state, "worker-id", token)
			if (err == nil) != (reply == `{"aborted":true}`) {
				t.Fatalf("ack result: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "private server detail")) {
				t.Fatal("private value in error")
			}
			request := <-requests
			if len(request) != 3 || request["action"] != "abort" || request["worker_id"] != "worker-id" || request["token"] != token {
				t.Fatal("changed private protocol")
			}
		})
	}
}

func TestAbortTokenStdinIsBoundedAndDoesNotEchoInput(t *testing.T) {
	for _, input := range []string{"synthetic-lease", "synthetic-lease\n", "synthetic-lease\r\n", strings.Repeat("a", 256) + "\r\n"} {
		if _, err := ReadAbortToken(strings.NewReader(input)); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []string{"", "\n", "two tokens", "first\nsecond", strings.Repeat("a", 257), strings.Repeat("a", 256) + "\r\nextra", "秘密"} {
		if _, err := ReadAbortToken(strings.NewReader(input)); err == nil {
			t.Fatal("accepted malformed token")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := AbortLease(ctx, "/unavailable/state.db", "worker", "synthetic-lease"); err == nil || strings.Contains(err.Error(), "synthetic-lease") {
		t.Fatal("cancelled request did not fail safely")
	}
}
