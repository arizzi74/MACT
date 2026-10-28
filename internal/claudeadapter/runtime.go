// Package claudeadapter adapts the Claude Code streaming CLI to the bounded
// app-server contract used by the gateway. Claude owns its persisted sessions;
// this package keeps a private display history and session inventory alongside
// them. It never evaluates a shell command or reads credentials from scripts.
package claudeadapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/iaia/telegramgw/internal/codexadapter"
)

const maxMessageBytes = 32 << 20

// LaunchSettings is read afresh for each native CLI process. Env values are
// child environment overrides, including ANTHROPIC_AUTH_TOKEN or
// ANTHROPIC_API_KEY as distinct authentication mechanisms. They are never
// returned through the protocol or written to the history store.
type LaunchSettings struct {
	Env            map[string]string
	Model          string
	SettingsFile   string
	PermissionMode string
}

type Config struct {
	Command          string
	Args             []string
	WorkingDirectory string
	StateDirectory   string
	SocketPath       string
	SettingsFile     string
	Env              []string
	Stderr           io.Writer
	ClientInfo       codexadapter.ClientInfo
	RequestTimeout   time.Duration
	StartupTimeout   time.Duration
	LoadSettings     func() (LaunchSettings, error)
	// Redact sanitizes structured provider error text before it becomes a
	// public turn error. Without it, only generic failure messages are used.
	Redact func(string) string
}

// Runtime owns the compatibility server, private local WebSocket endpoint,
// and native Claude subprocesses. Closing the management client also closes
// the runtime. A browser disconnect never terminates a native turn.
type Runtime struct {
	*codexadapter.Client
	SocketPath string
	server     *server
}

var supportedMethods = []string{
	"initialize", "thread/start", "thread/resume", "thread/read", "thread/list",
	"thread/loaded/list", "thread/name/set", "thread/turns/list", "turn/start",
	"turn/interrupt", "turn/steer", "thread/queue/list", "thread/queue/clear", "config/read", "model/list",
	"thread/status/read", "thread/delete", "thread/history/recover",
}

// SupportedMethods is an exhaustive allowlist. In particular Claude's queued
// streaming input is not used as native turn steering: turn/steer enqueues an
// adapter-owned successor turn. Goals, Codex
// account quotas, Codex permission profiles, fork/review and native attachments
// are not fabricated by the compatibility layer.
func SupportedMethods() []string { return append([]string(nil), supportedMethods...) }

type server struct {
	cfg               Config
	mu                sync.Mutex // Serializes admission, settings updates, and native events.
	threads           map[string]*savedThread
	deletions         map[string]*deletionRecord
	deleting          map[string]bool
	deletionErrors    map[string]string
	runners           map[string]*process
	pending           map[string]*permission
	peers             map[*peer]bool
	closed            bool
	ctx               context.Context
	cancel            context.CancelFunc
	listener          net.Listener
	http              *http.Server
	socketInfo        os.FileInfo
	closeOnce         sync.Once
	balance           *CreditBalance
	balanceGeneration uint64
	wg                sync.WaitGroup
}

type peer struct {
	server      *server
	queue       chan []byte
	done        chan struct{}
	closeOnce   sync.Once
	closeIO     func()
	initialized bool // Protected by server.mu.
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func Start(ctx context.Context, cfg Config) (*Runtime, error) {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.WorkingDirectory == "" {
		var err error
		cfg.WorkingDirectory, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	if !filepath.IsAbs(cfg.WorkingDirectory) {
		return nil, errors.New("Claude working directory must be absolute")
	}
	if cfg.StateDirectory == "" || !filepath.IsAbs(cfg.StateDirectory) {
		return nil, errors.New("Claude state directory must be absolute")
	}
	if err := privateDirectory(cfg.StateDirectory); err != nil {
		return nil, err
	}
	if err := validateSettingsFile(cfg.SettingsFile); err != nil {
		return nil, err
	}
	if err := validateArgs(cfg.Args); err != nil {
		return nil, err
	}
	threads, err := loadThreads(cfg.StateDirectory)
	if err != nil {
		return nil, err
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	s := &server{cfg: cfg, threads: threads, runners: make(map[string]*process), pending: make(map[string]*permission), peers: make(map[*peer]bool), ctx: serverCtx, cancel: cancel, deletions: make(map[string]*deletionRecord), deleting: make(map[string]bool), deletionErrors: make(map[string]string)}
	if err := s.recoverDeletions(); err != nil {
		cancel()
		return nil, err
	}
	settings, err := s.settings()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, thread := range threads {
		if s.deletions[thread.ID] != nil {
			continue
		}
		settleInterruptedTaskItems(thread)
		s.applyStatusSettings(thread, settings, false)
		if err := s.save(thread); err != nil {
			cancel()
			return nil, err
		}
	}
	if cfg.SocketPath != "" {
		if err := s.listen(cfg.SocketPath); err != nil {
			cancel()
			return nil, err
		}
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := s.newPeer(func() { _ = inR.Close(); _ = inW.Close(); _ = outR.Close(); _ = outW.Close() })
	s.wg.Add(2)
	go func() {
		defer s.wg.Done()
		defer p.close()
		scanner := bufio.NewScanner(inR)
		scanner.Buffer(make([]byte, 64<<10), maxMessageBytes)
		for scanner.Scan() {
			s.receive(p, scanner.Bytes())
		}
	}()
	go func() {
		defer s.wg.Done()
		defer p.close()
		for {
			select {
			case <-p.done:
				return
			case payload := <-p.queue:
				if _, err := outW.Write(append(payload, '\n')); err != nil {
					return
				}
			}
		}
	}()
	client := codexadapter.New(codexadapter.Transport{In: inW, Out: outR, Close: s.close}, codexadapter.Config{ClientInfo: cfg.ClientInfo, RequestTimeout: cfg.RequestTimeout, LocalSocket: cfg.SocketPath})
	r := &Runtime{Client: client, SocketPath: cfg.SocketPath, server: s}
	if err := client.Initialize(ctx); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	if r.Client != nil {
		_ = r.Client.Close()
	}
	if r.server != nil {
		err := r.server.close()
		r.server.wg.Wait()
		return err
	}
	return nil
}

func (r *Runtime) LocalSocket() string {
	if r == nil {
		return ""
	}
	return r.SocketPath
}

// WithIdleSettingsUpdate atomically fences turn admission while the caller
// persists new settings. Existing idle native processes are retired only after
// persistence succeeds. The next prompt resumes its native session with fresh
// settings. The callback must not call back into this runtime.
func (r *Runtime) WithIdleSettingsUpdate(update func() error) error {
	if r == nil || r.server == nil {
		return errors.New("Claude runtime is unavailable")
	}
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("Claude runtime is closed")
	}
	for _, record := range s.deletions {
		if !record.Done {
			return errors.New("Claude runtime has an incomplete session deletion; retry deleting that session first")
		}
	}
	if len(s.pending) != 0 {
		return errors.New("Claude runtime has pending approvals or questions")
	}
	for _, t := range s.threads {
		if t.active() != nil || len(t.Queue) > 0 {
			return errors.New("Claude runtime has an active turn")
		}
	}
	for _, p := range s.runners {
		if p.hasBackgroundWork() {
			return errors.New("Claude runtime has active background work")
		}
	}
	if update != nil {
		if err := update(); err != nil {
			return err
		}
	}
	for id, p := range s.runners {
		p.stop()
		delete(s.runners, id)
	}
	s.balanceGeneration++
	s.balance = nil
	settings, err := s.settings()
	if err != nil {
		return err
	}
	for _, thread := range s.threads {
		s.applyStatusSettings(thread, settings, true)
		if err := s.save(thread); err != nil {
			return err
		}
		s.broadcastStatus(thread)
	}
	return nil
}

func (r *Runtime) ReloadSettings(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.WithIdleSettingsUpdate(nil)
}

func (s *server) newPeer(closeIO func()) *peer {
	p := &peer{server: s, queue: make(chan []byte, 256), done: make(chan struct{}), closeIO: closeIO}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		p.close()
	} else {
		s.peers[p] = true
	}
	return p
}

func (p *peer) close() {
	p.closeOnce.Do(func() {
		close(p.done)
		if p.closeIO != nil {
			p.closeIO()
		}
	})
}

// A stalled client is disconnected instead of growing an unbounded queue or
// dropping lifecycle/approval events. Native execution continues independently.
func (p *peer) send(value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxMessageBytes {
		p.close()
		return
	}
	select {
	case <-p.done:
		return
	case p.queue <- data:
	default:
		p.close()
	}
}

func (s *server) broadcast(method string, params any) {
	for p := range s.peers {
		select {
		case <-p.done:
			delete(s.peers, p)
		default:
			if p.initialized {
				p.send(map[string]any{"method": method, "params": params})
			}
		}
	}
}

func (s *server) listen(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("Claude socket path must be absolute")
	}
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("Claude socket path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on Claude socket: %w", err)
	}
	if unix, ok := l.(*net.UnixListener); ok {
		unix.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		_ = l.Close()
		return err
	}
	s.listener, s.socketInfo = l, info
	s.http = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(s.serveWebSocket)}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); _ = s.http.Serve(l) }()
	return nil
}

func (s *server) serveWebSocket(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	conn, err := websocket.Accept(w, req, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(maxMessageBytes)
	p := s.newPeer(func() { _ = conn.CloseNow() })
	defer func() { s.mu.Lock(); delete(s.peers, p); s.mu.Unlock() }()
	defer p.close()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer p.close()
		for {
			select {
			case <-p.done:
				return
			case data := <-p.queue:
				writeCtx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
				err := conn.Write(writeCtx, websocket.MessageText, data)
				cancel()
				if err != nil {
					return
				}
			}
		}
	}()
	defer func() { p.close(); <-writerDone }()
	for {
		kind, data, err := conn.Read(s.ctx)
		if err != nil || kind != websocket.MessageText {
			return
		}
		s.receive(p, data)
	}
}

func (s *server) close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		s.closed = true
		for _, p := range s.runners {
			p.stop()
		}
		for p := range s.peers {
			p.close()
		}
		s.mu.Unlock()
		if s.http != nil {
			_ = s.http.Close()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
		if s.socketInfo != nil {
			if now, err := os.Lstat(s.cfg.SocketPath); err == nil && os.SameFile(now, s.socketInfo) {
				_ = os.Remove(s.cfg.SocketPath)
			}
		}
	})
	return nil
}

func initializeInfo(s *server) map[string]any {
	return map[string]any{"userAgent": "claude-code/telegramgw", "codexHome": s.cfg.StateDirectory, "platformFamily": runtime.GOOS, "platformOs": runtime.GOOS, "supportedMethods": SupportedMethods()}
}

func methodSupported(method string) bool {
	for _, m := range supportedMethods {
		if method == m {
			return true
		}
	}
	return false
}

func rpcError(p *peer, id json.RawMessage, code int, message string) {
	p.send(map[string]any{"id": id, "error": map[string]any{"code": code, "message": message}})
}

func rpcResult(p *peer, id json.RawMessage, result any) {
	p.send(map[string]any{"id": id, "result": result})
}

func sortedThreads(threads map[string]*savedThread) []*savedThread {
	items := make([]*savedThread, 0, len(threads))
	for _, t := range threads {
		items = append(items, t)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].UpdatedAt == items[j].UpdatedAt {
			return items[i].ID < items[j].ID
		}
		return items[i].UpdatedAt > items[j].UpdatedAt
	})
	return items
}
