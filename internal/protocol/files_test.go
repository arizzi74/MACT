package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestFileInputValidationAndOperations(t *testing.T) {
	for _, test := range []struct {
		name string
		file File
		bad  bool
	}{
		{"document", File{Name: "report.pdf", Data: []byte("pdf")}, false},
		{"empty document", File{Name: "empty.txt"}, false},
		{"exact 20 MiB", File{Name: "report.pdf", Data: make([]byte, 20<<20)}, false},
		{"unicode", File{Name: "résumé 例.txt", Data: []byte("text")}, false},
		{"missing name", File{Data: []byte("text")}, true},
		{"parent path", File{Name: "../report.pdf"}, true},
		{"windows path", File{Name: `a\report.pdf`}, true},
		{"absolute path", File{Name: "/report.pdf"}, true},
		{"control", File{Name: "report\n.pdf"}, true},
		{"nul", File{Name: "report\x00.pdf"}, true},
		{"too long", File{Name: strings.Repeat("a", 1025)}, true},
		{"20 MiB plus one byte", File{Name: "report.pdf", Data: make([]byte, (20<<20)+1)}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateFiles([]File{test.file}); (err != nil) != test.bad {
				t.Fatalf("validation=%v bad=%v", err, test.bad)
			}
		})
	}
	if ValidateFiles([]File{{Name: "a"}, {Name: "b"}}) == nil {
		t.Fatal("multiple files accepted")
	}
	for _, op := range []Operation{StartTurn, Steer, StoreFiles, Interrupt, NewSession, CodexCommand} {
		c := imageTestCommand()
		c.Operation, c.ExpectedTurnID = op, "turn"
		c.Arguments = Arguments{CWD: "/workspace", Files: []File{{Name: "report.pdf", Data: []byte("pdf")}}}
		valid := op == StartTurn || op == Steer || op == StoreFiles
		if err := c.Validate(); (err == nil) != valid {
			t.Fatalf("operation=%s validation=%v valid=%v", op, err, valid)
		}
	}
	c := imageTestCommand()
	c.Operation = StoreFiles
	c.Arguments = Arguments{CWD: "/workspace", Text: "caption", Files: []File{{Name: "report.pdf"}}}
	if c.Validate() == nil {
		t.Fatal("file saving accepted a caption without forwarding")
	}
	c.Arguments.Text = ""
	c.Arguments.CWD = ""
	if c.Validate() == nil {
		t.Fatal("file input accepted a mutable workspace")
	}
}

func TestMaximumFileFitsCompleteFrameAndRoundTrips(t *testing.T) {
	c := imageTestCommand()
	c.Arguments = Arguments{CWD: "/workspace", Text: strings.Repeat("caption ", 1024), Files: []File{{Name: "archive.zip", Data: bytes.Repeat([]byte{0xff}, MaxFileBytes)}}}
	envelope, err := NewEnvelope("command", c)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(envelope)
	if err != nil || len(frame) > MaxFrameBytes {
		t.Fatalf("frame=%d error=%v", len(frame), err)
	}
	decoded, err := Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	command, err := Payload[Command](decoded)
	if err != nil || len(command.Arguments.Files) != 1 || !bytes.Equal(command.Arguments.Files[0].Data, c.Arguments.Files[0].Data) {
		t.Fatalf("file did not round trip: %v", err)
	}
}

func TestWorkerFileCapacityDefaultsAndBounds(t *testing.T) {
	for _, test := range []struct {
		name                       string
		advertised, file, webInput int
	}{
		{"omitted legacy capability", 0, 10 << 20, 15 << 20},
		{"invalid negative capability", -1, 10 << 20, 15 << 20},
		{"legacy explicit capacity", 10 << 20, 10 << 20, 15 << 20},
		{"smaller capacity", 5 << 20, 5 << 20, 15 << 20},
		{"current capacity", 20 << 20, 20 << 20, 30 << 20},
		{"bounded advertised capacity", 100 << 20, 20 << 20, 30 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			if limit := WorkerFileByteLimit(test.advertised); limit != test.file {
				t.Fatalf("worker file limit=%d, want %d", limit, test.file)
			}
			if limit := WorkerWebUIInputByteLimit(test.advertised); limit != test.webInput {
				t.Fatalf("worker Web UI input limit=%d, want %d", limit, test.webInput)
			}
		})
	}
	for _, message := range []any{Hello{SupportsFileInput: true, MaxFileBytes: MaxFileBytes}, Heartbeat{SupportsFileInput: true, MaxFileBytes: MaxFileBytes}} {
		raw, err := json.Marshal(message)
		var capability struct {
			SupportsFileInput bool `json:"supports_file_input"`
			MaxFileBytes      int  `json:"max_file_bytes"`
		}
		if err != nil || json.Unmarshal(raw, &capability) != nil || !capability.SupportsFileInput || capability.MaxFileBytes != MaxFileBytes {
			t.Fatalf("file capacity missing from handshake/heartbeat: %s, %v", raw, err)
		}
	}
}
