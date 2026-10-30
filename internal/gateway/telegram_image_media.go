package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
)

// SendImage uploads bytes from the owning worker, never a public file URL.
// Images outside Telegram's photo geometry limits are sent as image documents.
func (t *TelegramClient) SendImage(ctx context.Context, chatID, topicID int64, value protocol.Image, caption string) (int64, error) {
	if chatID == 0 || topicID < 0 || protocol.ValidateImages([]protocol.Image{value}) != nil || telegramTextLength(caption) > 1024 {
		return 0, errors.New("invalid generated image")
	}
	extension := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}[value.MIMEType]
	method, field := "sendDocument", "document"
	if value.MIMEType != "image/webp" {
		config, _, err := image.DecodeConfig(bytes.NewReader(value.Data))
		if err != nil || config.Width < 1 || config.Height < 1 {
			return 0, errors.New("invalid generated image")
		}
		if value.MIMEType != "image/gif" && len(value.Data) <= 10_000_000 && config.Width+config.Height <= 10000 &&
			int64(config.Width) <= 20*int64(config.Height) && int64(config.Height) <= 20*int64(config.Width) {
			method, field = "sendPhoto", "photo"
		}
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10), "disable_notification": "true"}
	if caption != "" {
		fields["caption"] = caption
	}
	if topicID != 0 {
		fields["message_thread_id"] = strconv.FormatInt(topicID, 10)
	}
	for key, value := range fields {
		if err := form.WriteField(key, value); err != nil {
			return 0, errors.New("could not encode image message")
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="`+field+`"; filename="image.`+extension+`"`)
	header.Set("Content-Type", value.MIMEType)
	part, err := form.CreatePart(header)
	if err != nil {
		return 0, errors.New("could not encode image message")
	}
	if _, err = part.Write(value.Data); err != nil {
		return 0, errors.New("could not encode image message")
	}
	if err = form.Close(); err != nil {
		return 0, errors.New("could not encode image message")
	}
	defer clear(body.Bytes())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+"/bot"+t.token+"/"+method, &body)
	if err != nil {
		return 0, errors.New("could not prepare image upload")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	client := *t.http
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New("Telegram image upload failed")
	}
	defer response.Body.Close()
	var envelope struct {
		OK     bool `json:"ok"`
		Result struct {
			ID int64 `json:"message_id"`
		} `json:"result"`
		Code        int    `json:"error_code"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&envelope); err != nil {
		return 0, errors.New("invalid Telegram image response")
	}
	if !envelope.OK {
		return 0, &TelegramError{Code: envelope.Code, Description: strings.ReplaceAll(envelope.Description, t.token, "[REDACTED]"), RetryAfter: time.Duration(envelope.Parameters.RetryAfter) * time.Second}
	}
	if envelope.Result.ID <= 0 {
		return 0, errors.New("Telegram returned no image message identity")
	}
	return envelope.Result.ID, nil
}
