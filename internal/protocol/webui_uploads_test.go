package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWebUIUploadValidatesOneBasenameAndExactBytes(t *testing.T) {
	for _, data := range [][]byte{nil, {0, 1, 255, '\n'}, bytes.Repeat([]byte{0xfa}, MaxFileBytes)} {
		upload := WebUIUpload{UploadID: uuid.NewString(), Name: "report.pdf", Data: data}
		// []byte(nil) marshals as null; callers must explicitly send "" for an
		// empty file, just like FileReader's empty base64 result.
		raw, _ := json.Marshal(map[string]any{"uploadId": upload.UploadID, "name": upload.Name, "data": base64.StdEncoding.EncodeToString(data)})
		var params map[string]json.RawMessage
		_ = json.Unmarshal(raw, &params)
		decoded, err := DecodeWebUIUploadParams(params)
		if err != nil || decoded.UploadID != upload.UploadID || decoded.Name != upload.Name || !bytes.Equal(decoded.Data, data) {
			t.Fatalf("upload bytes/identity changed: size=%d, err=%v", len(data), err)
		}
		message, _ := json.Marshal(map[string]any{"id": 1, "method": "gateway/upload", "params": params})
		if len(message) > MaxWebUIInputBytes {
			t.Fatal("a maximum upload exceeds the browser input budget")
		}
		if _, err := NewEnvelope("webui", WebUIFrame{ID: uuid.NewString(), Action: "input", Data: message}); err != nil {
			t.Fatalf("upload exceeds worker transport budget: %v", err)
		}
	}
}

func TestWebUIUploadRejectsInvalidOrAmbiguousInput(t *testing.T) {
	id := uuid.NewString()
	valid := map[string]json.RawMessage{"uploadId": json.RawMessage(`"` + id + `"`), "name": json.RawMessage(`"report.bin"`), "data": json.RawMessage(`"AA=="`)}
	for _, test := range []struct{ key, value string }{
		{"path", `"/tmp/report"`}, {"threadId", `"other"`}, {"files", `[]`},
		{"uploadId", `null`}, {"uploadId", `"invalid"`}, {"uploadId", `"` + strings.ToUpper(id) + `"`},
		{"name", `"../report"`}, {"name", `"a\\b"`}, {"name", `".."`}, {"name", `""`}, {"name", `"a\n"`},
		{"data", `null`}, {"data", `[]`}, {"data", `"A A=="`}, {"data", `"AA==\n"`}, {"data", `"AB=="`}, {"data", `"AA"`},
		{"data", `"` + base64.StdEncoding.EncodeToString(make([]byte, MaxFileBytes+1)) + `"`},
	} {
		params := map[string]json.RawMessage{}
		for key, value := range valid {
			params[key] = value
		}
		params[test.key] = json.RawMessage(test.value)
		if _, err := DecodeWebUIUploadParams(params); err == nil {
			t.Errorf("unsafe upload accepted for %s", test.key)
		}
	}
	for key := range valid {
		params := map[string]json.RawMessage{}
		for other, value := range valid {
			if key != other {
				params[other] = value
			}
		}
		if _, err := DecodeWebUIUploadParams(params); err == nil {
			t.Errorf("upload accepted without %s", key)
		}
	}
}
