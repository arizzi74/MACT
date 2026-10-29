package workerupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"
)

// ControlPath stays in the worker state directory, including when the Unix
// socket name must be shortened. It works across the service's PrivateTmp.
func ControlPath(stateFile string) string {
	path := stateFile + ".control.sock"
	if len(path) <= 100 {
		return path
	}
	digest := sha256.Sum256([]byte(stateFile))
	return filepath.Join(filepath.Dir(stateFile), fmt.Sprintf("%x.sock", digest[:4]))
}

// AbortLease uses the existing private worker control protocol directly. No
// subprocess argv, environment, temporary file or log carries the lease token.
// The wire format is also understood by workers predating this client.
func AbortLease(ctx context.Context, stateFile, workerID, token string) error {
	path := ControlPath(stateFile)
	if !filepath.IsAbs(stateFile) || len(path) > 100 || workerID == "" || !validAbortToken(token) {
		return errors.New("worker update: invalid abort request")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return errors.New("worker update: private control socket unavailable")
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	request := struct {
		Action   string `json:"action"`
		WorkerID string `json:"worker_id"`
		Token    string `json:"token"`
	}{"abort", workerID, token}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return errors.New("worker update: abort request could not be sent")
	}
	var response struct {
		Aborted bool   `json:"aborted"`
		Error   string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(connection, 8192)).Decode(&response) != nil || !response.Aborted || response.Error != "" {
		return errors.New("worker update: abort was not acknowledged")
	}
	return nil
}

// ReadAbortToken is the explicit stdin-only manual CLI path. Read a bounded
// token and never include the supplied input in an error.
func ReadAbortToken(input io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(input, 259))
	token := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if err != nil || len(data) > 258 || !validAbortToken(token) {
		return "", errors.New("worker update: expected one bounded lease token on stdin")
	}
	return token, nil
}

func validAbortToken(token string) bool {
	if len(token) == 0 || len(token) > 256 {
		return false
	}
	for _, char := range token {
		if char < '!' || char > '~' {
			return false
		}
	}
	return true
}
