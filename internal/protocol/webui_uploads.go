package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
)

// WebUIUpload is storage-only. Its destination is derived from the selected
// session by the worker; the browser cannot supply a filesystem path.
type WebUIUpload struct {
	UploadID string `json:"uploadId"`
	Name     string `json:"name"`
	Data     []byte `json:"data"`
}

type WebUIUploadResult struct {
	UploadID string `json:"uploadId"`
	Path     string `json:"path"`
	Text     string `json:"text"`
}

func (u WebUIUpload) Validate() error {
	id, err := uuid.Parse(u.UploadID)
	if err != nil || id.String() != u.UploadID {
		return errors.New("A canonical UUID uploadId is required.")
	}
	if err := ValidateFiles([]File{{Name: u.Name, Data: u.Data}}); err != nil {
		return errors.New("Upload one file with a valid basename and at most 10 MiB.")
	}
	return nil
}

// DecodeWebUIUploadParams rejects paths, extra files, null values and lenient
// base64 forms before any bytes reach the worker's filesystem.
func DecodeWebUIUploadParams(params map[string]json.RawMessage) (WebUIUpload, error) {
	var upload WebUIUpload
	var encoded *string
	if len(params) != 3 || json.Unmarshal(params["uploadId"], &upload.UploadID) != nil ||
		json.Unmarshal(params["name"], &upload.Name) != nil || json.Unmarshal(params["data"], &encoded) != nil || encoded == nil {
		return upload, errors.New("An uploadId, basename and base64 data are required for one file.")
	}
	if len(*encoded) > base64.StdEncoding.EncodedLen(MaxFileBytes) || strings.ContainsAny(*encoded, "\r\n\t ") {
		return upload, errors.New("Upload one file of at most 10 MiB using valid base64 data.")
	}
	var err error
	upload.Data, err = base64.StdEncoding.Strict().DecodeString(*encoded)
	if err != nil {
		return WebUIUpload{}, errors.New("The upload contains invalid base64 data.")
	}
	if err := upload.Validate(); err != nil {
		return WebUIUpload{}, err
	}
	return upload, nil
}
