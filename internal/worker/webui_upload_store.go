package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
)

var bucketWebUIUploads = []byte("webui_uploads")

// Upload metadata is private local state. It contains no file bytes and never
// enters command recovery, native replay, history or the Telegram event outbox.
type webUIUploadScope struct {
	RuntimeID         string `json:"runtime_id"`
	RuntimeGeneration uint64 `json:"runtime_generation"`
	SessionID         string `json:"session_id"`
	ThreadID          string `json:"thread_id"`
	CWD               string `json:"cwd"`
}

type webUIUploadRecord struct {
	Scope  webUIUploadScope   `json:"scope"`
	Name   string             `json:"name"`
	Digest string             `json:"digest"`
	File   *inboundFileRecord `json:"file,omitempty"`
}

func storeWebUIUpload(store *Store, runtime protocol.Runtime, session protocol.Session, upload protocol.WebUIUpload, roots []string) (string, error) {
	if err := upload.Validate(); err != nil {
		return "", err
	}
	digest := sha256.Sum256(upload.Data)
	identity := webUIUploadRecord{
		Scope: webUIUploadScope{RuntimeID: runtime.ID, RuntimeGeneration: runtime.Generation, SessionID: session.ID, ThreadID: session.ThreadID, CWD: session.CWD},
		Name:  upload.Name, Digest: hex.EncodeToString(digest[:]),
	}
	var meta *inboundFileRecord
	// Reserve the upload ID before opening or creating anything in the workspace.
	// Reusing it under another session/runtime/CWD or with changed bytes fails
	// even when the sanitized filename would happen to be identical.
	if err := store.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketWebUIUploads)
		if value := bucket.Get([]byte(upload.UploadID)); value != nil {
			var previous webUIUploadRecord
			if json.Unmarshal(value, &previous) != nil {
				return errors.New("The worker cannot read the upload record.")
			}
			if previous.Scope != identity.Scope || previous.Name != identity.Name || previous.Digest != identity.Digest {
				return errors.New("This upload ID belongs to another file or session target. Attach the file again to create a new upload.")
			}
			meta = previous.File
			return nil
		}
		encoded, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(upload.UploadID), encoded)
	}); err != nil {
		return "", err
	}
	update := func(file *inboundFileRecord, published bool) error {
		return store.db.Update(func(tx *workerdb.Tx) error {
			bucket := tx.Bucket(bucketWebUIUploads)
			var record webUIUploadRecord
			if json.Unmarshal(bucket.Get([]byte(upload.UploadID)), &record) != nil || record.Scope != identity.Scope || record.Name != identity.Name || record.Digest != identity.Digest {
				return errors.New("The upload record changed.")
			}
			if published {
				if record.File == nil {
					return errors.New("The uploaded file is not recorded.")
				}
				record.File.Published = true
			} else {
				if record.File != nil {
					return errors.New("The uploaded file is already recorded.")
				}
				record.File = file
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			return bucket.Put([]byte(upload.UploadID), encoded)
		})
	}
	path, _, err := publishInboundFile(protocol.File{Name: upload.Name, Data: upload.Data}, upload.UploadID, session.CWD, roots, meta, "",
		func(file *inboundFileRecord) error { return update(file, false) },
		func() error { return update(nil, true) })
	return path, err
}
