package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	stdimage "image"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

type TelegramImageSender interface {
	SendImage(context.Context, int64, int64, protocol.Image, string) (int64, error)
}

type TelegramFileOpener interface {
	OpenWebUIFile(context.Context, registry.AdminSession, protocol.WebUIFileRequest) (*WebUIStream, error)
}

type telegramImageStore interface {
	TelegramImageTarget(context.Context, registry.Delivery) (registry.AdminSession, error)
	CheckWebUIFileTarget(context.Context, registry.AdminSession) error
}

type telegramImageCheckpoint struct {
	Path string `json:"path"`
}

type telegramImageStream interface {
	Frames() <-chan protocol.WebUIFrame
	Done() <-chan struct{}
	Consumed(protocol.WebUIFrame)
	Send(context.Context, json.RawMessage) error
}

// Only explicit image references in a final answer become attachments. Code
// blocks, arbitrary tool output, remote URLs and non-image files are excluded.
func telegramImagePaths(source string) []string {
	if len(source) > telegramMarkdownMaxSource {
		return nil
	}
	data := []byte(source)
	root := goldmark.New().Parser().Parse(text.NewReader(data))
	seen := map[string]bool{}
	var result []string
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		var raw string
		switch value := node.(type) {
		case *ast.Link:
			raw = string(telegramMarkdownUnescape(value.Destination))
		case *ast.Image:
			raw = string(telegramMarkdownUnescape(value.Destination))
		case *ast.CodeSpan:
			raw = string(value.Text(data))
			if space := strings.IndexAny(raw, " \t\n"); space >= 0 {
				if slash := strings.Index(raw, "/"); slash < 0 || slash > space {
					return ast.WalkSkipChildren, nil
				}
			}
		default:
			return ast.WalkContinue, nil
		}
		local, err := protocol.NormalizeWebUIFilePath(raw)
		if err != nil || strings.HasPrefix(local, "//") || strings.Contains(local, ":") {
			return ast.WalkSkipChildren, nil
		}
		switch strings.ToLower(path.Ext(local)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		default:
			return ast.WalkSkipChildren, nil
		}
		for _, part := range strings.Split(local, "/") {
			if part == ".." {
				return ast.WalkSkipChildren, nil
			}
		}
		local = path.Clean(local)
		if !seen[local] {
			seen[local] = true
			result = append(result, local)
		}
		if len(result) == 4 {
			return ast.WalkStop, nil
		}
		return ast.WalkSkipChildren, nil
	})
	return result
}

func (s *Sender) appendImageCheckpoints(row registry.Delivery, messages []json.RawMessage) []json.RawMessage {
	if s.options.Files == nil {
		return messages
	}
	if _, ok := s.api.(TelegramImageSender); !ok {
		return messages
	}
	if _, ok := s.store.(telegramImageStore); !ok {
		return messages
	}
	var event protocol.Event
	var result protocol.Result
	if json.Unmarshal(row.Payload, &event) != nil || json.Unmarshal(event.Data, &result) != nil {
		return messages
	}
	source := result.Text
	if s.options.Redactor != nil {
		source = s.options.Redactor.Redact(source)
	}
	for _, local := range telegramImagePaths(source) {
		message := sessionDeliveryMessage{SendMessage: SendMessage{ChatID: row.ChatID, TopicID: row.TopicID}, Image: &telegramImageCheckpoint{Path: local}}
		raw, err := json.Marshal(message)
		if err == nil {
			messages = append(messages, raw)
		}
	}
	return messages
}

func (s *Sender) sendImageCheckpoint(ctx context.Context, row registry.Delivery, image *telegramImageCheckpoint) (int64, error) {
	fallback := func() (int64, error) {
		if skip, err := s.skipInvisibleDelivery(ctx, row); err != nil {
			return 0, err
		} else if skip {
			return 0, errSessionDeliverySuppressed
		}
		return s.sendAuthorizedMessage(ctx, row, SendMessage{ChatID: row.ChatID, TopicID: row.TopicID, Text: "The image could not be attached. Open its file link in the MACT Web UI to download it."})
	}
	store, ok := s.store.(telegramImageStore)
	if !ok || s.options.Files == nil {
		return fallback()
	}
	target, err := store.TelegramImageTarget(ctx, row)
	if err != nil {
		return fallback()
	}
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stream, err := s.options.Files.OpenWebUIFile(readCtx, target, protocol.WebUIFileRequest{Path: image.Path})
	if err != nil {
		return fallback()
	}
	defer stream.Close()
	check := func() error {
		if skip, err := s.skipInvisibleDelivery(ctx, row); err != nil {
			return err
		} else if skip {
			return errSessionDeliverySuppressed
		}
		if err := stream.Check(readCtx); err != nil {
			return err
		}
		if _, err := store.TelegramImageTarget(readCtx, row); err != nil {
			return err
		}
		return store.CheckWebUIFileTarget(readCtx, target)
	}
	content, err := readTelegramImage(readCtx, stream, check)
	if err != nil {
		if errors.Is(err, errSessionDeliverySuppressed) {
			return 0, err
		}
		return fallback()
	}
	defer clear(content.Data)
	if err := check(); err != nil {
		return 0, err
	}
	presentation, err := s.deliveryPresentation(ctx, row)
	if err != nil {
		return 0, err
	}
	caption := ""
	if presentation.MultiSession {
		var label strings.Builder
		units := 0
		for _, r := range strings.TrimSpace(presentation.Marker + " " + presentation.Name) {
			width := 1
			if r > 0xffff {
				width = 2
			}
			if units+width > 1000 {
				break
			}
			label.WriteRune(r)
			units += width
		}
		caption = label.String()
	}
	if err := check(); err != nil {
		return 0, err
	}
	// Bot credentials and destination permissions are resolved at the actual
	// upload boundary too, after the file read's asynchronous round trips.
	if err := s.authorizeDeliveryDestination(ctx, row, row.ChatID); err != nil {
		return 0, err
	}
	api, ok := s.api.(TelegramImageSender)
	if !ok {
		return fallback()
	}
	id, err := api.SendImage(ctx, row.ChatID, row.TopicID, content, caption)
	var telegramError *TelegramError
	if errors.As(err, &telegramError) && telegramError.Code >= 400 && telegramError.Code < 500 && telegramError.Code != 429 {
		return fallback()
	}
	return id, err
}

func readTelegramImage(ctx context.Context, stream telegramImageStream, check func() error) (protocol.Image, error) {
	read := func() (protocol.WebUIFrame, error) {
		select {
		case <-ctx.Done():
			return protocol.WebUIFrame{}, ctx.Err()
		case <-stream.Done():
			return protocol.WebUIFrame{}, errors.New("image transfer ended")
		case frame := <-stream.Frames():
			stream.Consumed(frame)
			if err := check(); err != nil {
				return protocol.WebUIFrame{}, err
			}
			if frame.Action == "error" {
				return protocol.WebUIFrame{}, errors.New("image unavailable")
			}
			return frame, nil
		}
	}
	frame, err := read()
	if err != nil {
		return protocol.Image{}, err
	}
	var info protocol.WebUIFileInfo
	if frame.Action != "ready" || json.Unmarshal(frame.Data, &info) != nil || info.Size < 1 || info.Size > protocol.MaxImageBytes {
		return protocol.Image{}, errors.New("invalid image size")
	}
	data := make([]byte, 0, int(info.Size))
	ok := false
	defer func() {
		if !ok {
			clear(data)
		}
	}()
	for {
		offset := int64(len(data))
		credit, _ := json.Marshal(protocol.WebUIFileRead{Offset: offset})
		if err := stream.Send(ctx, credit); err != nil {
			return protocol.Image{}, err
		}
		frame, err := read()
		if err != nil {
			return protocol.Image{}, err
		}
		var chunk protocol.WebUIFileChunk
		if frame.Action != "output" || json.Unmarshal(frame.Data, &chunk) != nil || chunk.Offset != offset || len(chunk.Data) > protocol.WebUIFileChunkBytes || int64(len(chunk.Data)) > info.Size-offset || len(chunk.Data) == 0 && !chunk.EOF || chunk.EOF != (offset+int64(len(chunk.Data)) == info.Size) {
			return protocol.Image{}, errors.New("invalid image transfer")
		}
		data = append(data, chunk.Data...)
		clear(chunk.Data)
		if chunk.EOF {
			break
		}
	}
	image := protocol.Image{MIMEType: http.DetectContentType(data), Data: data}
	if err := protocol.ValidateImages([]protocol.Image{image}); err != nil {
		return protocol.Image{}, err
	}
	if image.MIMEType != "image/webp" {
		if _, _, err := stdimage.DecodeConfig(bytes.NewReader(data)); err != nil {
			return protocol.Image{}, err
		}
	}
	ok = true
	return image, nil
}
