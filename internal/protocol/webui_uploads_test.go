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
		frame := WebUIFrame{ID: uuid.NewString(), Action: "input", Data: message}
		if err := frame.Validate(); err != nil {
			t.Fatalf("upload exceeds relay input budget: %v", err)
		}
		envelope, err := NewEnvelope("webui", frame)
		if err != nil {
			t.Fatalf("upload exceeds worker transport budget: %v", err)
		}
		encoded, err := json.Marshal(envelope)
		if err != nil || len(encoded) > MaxFrameBytes {
			t.Fatalf("upload exceeds complete transport frame: bytes=%d, err=%v", len(encoded), err)
		}
		decodedEnvelope, err := Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		received, err := Payload[WebUIFrame](decodedEnvelope)
		if err != nil || !bytes.Equal(received.Data, message) {
			t.Fatalf("upload changed in nested transport envelope: %v", err)
		}
	}
}

func TestFileInputTransportLimitsKeepImageLimitIndependent(t *testing.T) {
	if MaxFileBytes != 20<<20 || MaxImageBytes != 10<<20 {
		t.Fatalf("unexpected attachment limits: files=%d, images=%d", MaxFileBytes, MaxImageBytes)
	}
	if base64.StdEncoding.EncodedLen(MaxFileBytes)+(64<<10) > MaxWebUIInputBytes || MaxWebUIInputBytes+(64<<10) > MaxFrameBytes {
		t.Fatal("maximum file does not leave room for its JSON and transport wrappers")
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
