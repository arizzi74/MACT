package protocol

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxFileBytes = 20 << 20
const LegacyMaxFileBytes = 10 << 20
const MaxFileCount = 1

// WorkerFileByteLimit defaults omitted capabilities to the legacy file limit
// and never lets a worker raise the gateway's global attachment ceiling.
func WorkerFileByteLimit(advertised int) int {
	if advertised <= 0 {
		return LegacyMaxFileBytes
	}
	return min(advertised, MaxFileBytes)
}

// File contains immutable document bytes and an untrusted original basename.
// A single base64 encoded 20 MiB file fits the 32 MiB transport frame limit.
type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

func ValidateFiles(files []File) error {
	if len(files) > MaxFileCount {
		return errors.New("too many input files")
	}
	for _, file := range files {
		if len(file.Data) > MaxFileBytes {
			return errors.New("input file size is invalid")
		}
		if strings.TrimSpace(file.Name) == "" || len(file.Name) > 1024 || !utf8.ValidString(file.Name) ||
			strings.ContainsAny(file.Name, "/\\") || strings.IndexFunc(file.Name, unicode.IsControl) >= 0 || file.Name == "." || file.Name == ".." {
			return errors.New("input file name is invalid")
		}
	}
	return nil
}

// InboundFilename derives the worker-owned destination basename after the
// original basename and UUID have been validated. It never accepts a path.
func InboundFilename(name, commandID string) string {
	clean := strings.Map(func(ch rune) rune {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '.' || ch == '-' || ch == '_' {
			return ch
		}
		return '_'
	}, name)
	clean = strings.Trim(clean, "._-")
	if clean == "" {
		clean = "file"
	}
	ext := filepath.Ext(clean)
	if len(ext) > 32 {
		ext = ""
	}
	stem := strings.TrimSuffix(clean, ext)
	if len(stem) > 150 {
		stem = stem[:150]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
	}
	return stem + "-" + commandID + ext
}
