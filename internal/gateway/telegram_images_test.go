package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

func outgoingPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	image := image.NewPaletted(image.Rect(0, 0, width, height), color.Palette{color.Black, color.White})
	if err := png.Encode(&out, image); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type imageDeliveryStore struct {
	*checkpointStore
	target       registry.AdminSession
	targetError  error
	presentation registry.TelegramSessionPresentation
}

func (s *imageDeliveryStore) TelegramImageTarget(context.Context, registry.Delivery) (registry.AdminSession, error) {
	return s.target, s.targetError
}
func (s *imageDeliveryStore) TelegramSessionPresentation(context.Context, string, int64, int64, string) (registry.TelegramSessionPresentation, error) {
	return s.presentation, nil
}
func (s *imageDeliveryStore) CheckWebUIFileTarget(context.Context, registry.AdminSession) error {
	return nil
}
func (s *imageDeliveryStore) PrepareDeliveryChunks(_ context.Context, _ string, values []json.RawMessage) ([]registry.DeliveryChunk, error) {
	if len(s.chunks) == 0 {
		for index, raw := range values {
			s.chunks = append(s.chunks, registry.DeliveryChunk{Index: index, Payload: raw})
		}
	}
	return s.chunks, nil
}
func (s *imageDeliveryStore) MarkDeliveryChunkSent(_ context.Context, _ string, index int, _ int64, _, _, _ string, _ ...string) error {
	s.chunks[index].Sent = true
	s.marked = append(s.marked, index)
	return nil
}

type imageDeliveryAPI struct {
	*deliveryAPI
	images      []protocol.Image
	captions    []string
	order       []string
	rateLimited bool
}

func (a *imageDeliveryAPI) Send(ctx context.Context, message SendMessage) (int64, error) {
	a.order = append(a.order, "text")
	return a.deliveryAPI.Send(ctx, message)
}
func (a *imageDeliveryAPI) SendImage(_ context.Context, chat, topic int64, value protocol.Image, caption string) (int64, error) {
	if chat != 99 || topic != 3 {
		return 0, errors.New("incorrect frozen image destination")
	}
	if a.rateLimited {
		a.rateLimited = false
		return 0, &TelegramError{Code: 429, RetryAfter: time.Second}
	}
	a.order = append(a.order, "image")
	a.captions = append(a.captions, caption)
	a.images = append(a.images, protocol.Image{MIMEType: value.MIMEType, Data: append([]byte(nil), value.Data...)})
	return 100, nil
}

type imageConnectionRegistry struct{ WorkerRegistry }

func (imageConnectionRegistry) CheckConnection(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}

func TestTelegramImageDeliveryUsesWorkerRelayAndResumesAfterTextWithoutReplay(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	row := eventRow(t, "turn_completed", protocol.Result{TurnID: "image-turn", Text: "Here is the screenshot: ![Screen](./shots/screen.png)"}, testSessionID.String())
	row.ChatID, row.TopicID = 99, 3
	store := &imageDeliveryStore{checkpointStore: &checkpointStore{DeliveryStore: renderFixture(), row: row}, target: registry.AdminSession{Session: protocol.Session{ID: testSessionID.String(), WorkerID: testWorkerID.String(), RuntimeID: testRuntimeID.String(), ThreadID: "native-thread", CWD: "/project"}, RuntimeGeneration: 1}}
	store.presentation = registry.TelegramSessionPresentation{MultiSession: true, Name: "auth-fix", Marker: "🟩"}
	hub := NewHub(imageConnectionRegistry{}, nil, time.Second, time.Second)
	p := &peer{workerID: testWorkerID, connectionID: uuid.New(), supportsWebUIFiles: true, ctx: ctx, cancel: cancel, writes: make(chan writeRequest, 8)}
	hub.peers[testWorkerID.String()] = p
	data := outgoingPNG(t, 8, 8)
	done := make(chan struct{})
	failures := make(chan error, 8)
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case request := <-p.writes:
				frame, err := protocol.Payload[protocol.WebUIFrame](request.envelope)
				request.finish(err)
				if err != nil {
					failures <- err
					continue
				}
				var reply protocol.WebUIFrame
				switch frame.Action {
				case "file_open":
					var input protocol.WebUIFileRequest
					_ = json.Unmarshal(frame.Data, &input)
					if input.Path != "shots/screen.png" || frame.SessionID != testSessionID.String() || frame.RuntimeID != testRuntimeID.String() || frame.RuntimeGeneration != 1 {
						failures <- errors.New("image read retargeted")
					}
					raw, _ := json.Marshal(protocol.WebUIFileInfo{Name: "screen.png", Path: "/project/shots/screen.png", Size: int64(len(data))})
					reply = protocol.WebUIFrame{ID: frame.ID, Action: "ready", Data: raw}
				case "input":
					var input protocol.WebUIFileRead
					_ = json.Unmarshal(frame.Data, &input)
					raw, _ := json.Marshal(protocol.WebUIFileChunk{Offset: input.Offset, Data: data, EOF: true})
					reply = protocol.WebUIFrame{ID: frame.ID, Action: "output", Data: raw}
				default:
					continue
				}
				envelope, _ := protocol.NewEnvelope("webui", reply)
				if err := hub.handleWebUI(p, envelope); err != nil {
					failures <- err
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		close(failures)
		for err := range failures {
			t.Error(err)
		}
	})
	api := &imageDeliveryAPI{deliveryAPI: &deliveryAPI{}, rateLimited: true}
	sender := NewSender(store, api, nil, SenderOptions{Files: hub, BotID: "bot", OwnerID: 42})
	if err := sender.flush(ctx); err == nil {
		t.Fatal("image rate limit was ignored")
	}
	if len(api.messages) != 1 || len(api.images) != 0 || store.delay != time.Second {
		t.Fatal("text/attachment retry lost checkpoint", len(api.messages), len(api.images), store.delay)
	}
	if err := sender.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sender.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.order, []string{"text", "image"}) || len(api.images) != 1 || !bytes.Equal(api.images[0].Data, data) {
		t.Fatal("duplicate, wrong order or corrupt image", api.order)
	}
	if !reflect.DeepEqual(api.captions, []string{"🟩 auth-fix"}) {
		t.Fatal("missing multisession image identity", api.captions)
	}
}

type forbiddenImageOpener struct{}

func (forbiddenImageOpener) OpenWebUIFile(context.Context, registry.AdminSession, protocol.WebUIFileRequest) (*WebUIStream, error) {
	return nil, errors.New("must not open unauthorized target")
}

func TestTelegramImageUnavailableDoesNotBlockOrReplayFinalText(t *testing.T) {
	row := eventRow(t, "turn_completed", protocol.Result{Text: "![Screenshot](./screen.png)"}, testSessionID.String())
	row.ChatID, row.TopicID = 99, 3
	store := &imageDeliveryStore{checkpointStore: &checkpointStore{DeliveryStore: renderFixture(), row: row}, targetError: registry.ErrWebUIFileTarget}
	api := &imageDeliveryAPI{deliveryAPI: &deliveryAPI{}}
	sender := NewSender(store, api, nil, SenderOptions{Files: forbiddenImageOpener{}, BotID: "bot", OwnerID: 42})
	for range 2 {
		if err := sender.flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(api.images) != 0 || len(api.messages) != 2 || !strings.Contains(api.messages[1].Text, "could not be attached") {
		t.Fatal("unavailable attachment blocked text", len(api.messages), api.order)
	}
}

func TestTelegramImageReferencesAreLocalExplicitBoundedAndDeduplicated(t *testing.T) {
	source := "![Screen](./shots/first.png) [Download](shots/first.png) `./shots/second.JPG` " +
		"[Spaced](<./shots/Final screen.png>) ![External](https://example.com/private.png) " +
		"[Not image](./report.pdf) [Traversal](../secret.png) `cat shots/command.png` " +
		"`javascript:evil.png`\n\n```sh\ncat ./ignored.png\n```\n\n`last.webp` `excess.png`"
	want := []string{"shots/first.png", "shots/second.JPG", "shots/Final screen.png", "last.webp"}
	if got := telegramImagePaths(source); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	for _, source := range []string{"`/secret\x00.png`", "[remote](//server/secret.png)", "[path](file://remote/secret.png)", strings.Repeat("x", telegramMarkdownMaxSource+1)} {
		if len(telegramImagePaths(source)) != 0 {
			t.Fatal("unsafe source produced an attachment")
		}
	}
}

type imageStreamFixture struct {
	frames    chan protocol.WebUIFrame
	done      chan struct{}
	data      []byte
	badOffset bool
	reads     int
}

func (f *imageStreamFixture) Frames() <-chan protocol.WebUIFrame { return f.frames }
func (f *imageStreamFixture) Done() <-chan struct{}              { return f.done }
func (f *imageStreamFixture) Consumed(protocol.WebUIFrame)       {}
func (f *imageStreamFixture) Send(_ context.Context, raw json.RawMessage) error {
	var request protocol.WebUIFileRead
	if err := json.Unmarshal(raw, &request); err != nil {
		return err
	}
	f.reads++
	end := min(int(request.Offset)+protocol.WebUIFileChunkBytes, len(f.data))
	chunk := protocol.WebUIFileChunk{Offset: request.Offset, Data: f.data[request.Offset:end], EOF: end == len(f.data)}
	if f.badOffset {
		chunk.Offset++
	}
	encoded, _ := json.Marshal(chunk)
	f.frames <- protocol.WebUIFrame{Action: "output", Data: encoded}
	return nil
}
func newImageStream(data []byte, size int64) *imageStreamFixture {
	f := &imageStreamFixture{frames: make(chan protocol.WebUIFrame, 1), done: make(chan struct{}), data: data}
	raw, _ := json.Marshal(protocol.WebUIFileInfo{Name: "screenshot.png", Path: "/project/screenshot.png", Size: size})
	f.frames <- protocol.WebUIFrame{Action: "ready", Data: raw}
	return f
}

func TestTelegramImageReadsBinaryCreditsAndRechecksAuthorization(t *testing.T) {
	data := append(outgoingPNG(t, 8, 8), bytes.Repeat([]byte{0xfe}, 2*protocol.WebUIFileChunkBytes)...)
	f := newImageStream(data, int64(len(data)))
	checks := 0
	image, err := readTelegramImage(t.Context(), f, func() error { checks++; return nil })
	if err != nil || image.MIMEType != "image/png" || !bytes.Equal(image.Data, data) || f.reads != 3 || checks != 4 {
		t.Fatal("corrupt/unbounded transfer", image.MIMEType, len(image.Data), checks, f.reads, err)
	}
	for _, cause := range []string{"oversize", "offset", "mime", "revoked", "connection"} {
		t.Run(cause, func(t *testing.T) {
			data := outgoingPNG(t, 8, 8)
			f := newImageStream(data, int64(len(data)))
			checks := 0
			if cause == "oversize" {
				f = newImageStream(nil, protocol.MaxImageBytes+1)
			}
			if cause == "offset" {
				f.badOffset = true
			}
			if cause == "mime" {
				f = newImageStream([]byte("not an image"), 12)
			}
			if cause == "connection" {
				close(f.done)
				<-f.frames
			}
			image, err := readTelegramImage(t.Context(), f, func() error {
				checks++
				if cause == "revoked" && checks > 1 {
					return errSessionDeliverySuppressed
				}
				return nil
			})
			if err == nil || image.Data != nil {
				t.Fatal("unsafe image accepted", cause, err)
			}
		})
	}
}

func TestSendGeneratedImageMultipartAndDocumentFallback(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		large         bool
		method, field string
	}{
		{"screenshot", 16, 16, false, "sendPhoto", "photo"},
		{"panorama", 150, 1, false, "sendDocument", "document"},
		{"photo size", 16, 16, true, "sendDocument", "document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := outgoingPNG(t, tc.width, tc.height)
			if tc.large {
				data = append(data, make([]byte, 10_000_001-len(data))...)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/botttoken/"+tc.method {
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
				}
				reader, err := r.MultipartReader()
				if err != nil {
					t.Error(err)
					return
				}
				fields := map[string]string{}
				files := 0
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Error(err)
						return
					}
					body, _ := io.ReadAll(part)
					if part.FileName() != "" {
						files++
						if part.FormName() != tc.field || part.FileName() != "image.png" || part.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, data) {
							t.Error("image bytes or MIME changed")
						}
					} else {
						fields[part.FormName()] = string(body)
					}
				}
				if files != 1 || fields["chat_id"] != "7" || fields["message_thread_id"] != "11" || fields["caption"] != "Session 🧪" {
					t.Error("image destination changed", fields, files)
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
			}))
			defer server.Close()
			api := NewTelegramClient("ttoken")
			api.endpoint = server.URL
			id, err := api.SendImage(t.Context(), 7, 11, protocol.Image{MIMEType: "image/png", Data: data}, "Session 🧪")
			if err != nil || id != 42 {
				t.Fatal(id, err)
			}
		})
	}
}

func TestSendGeneratedImageErrorsRedactionAndRedirect(t *testing.T) {
	redirected := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++ }))
	defer target.Close()
	for _, mode := range []string{"redirect", "rate limit", "no identity", "invalid JSON"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					http.Redirect(w, r, target.URL, 307)
				case "rate limit":
					_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"token secret-token","parameters":{"retry_after":7}}`)
				case "no identity":
					_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":0}}`)
				default:
					_, _ = io.WriteString(w, `not JSON`)
				}
			}))
			defer server.Close()
			api := NewTelegramClient("secret-token")
			api.endpoint = server.URL
			_, err := api.SendImage(t.Context(), 7, 0, protocol.Image{MIMEType: "image/png", Data: outgoingPNG(t, 8, 8)}, "")
			if err == nil || strings.Contains(err.Error(), "secret-token") {
				t.Fatal(err)
			}
			if mode == "rate limit" {
				var failure *TelegramError
				if !errors.As(err, &failure) || failure.RetryAfter != 7*time.Second {
					t.Fatal(err)
				}
			}
		})
	}
	if redirected != 0 {
		t.Fatal("uploaded private image to redirect destination")
	}
}
