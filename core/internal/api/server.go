// Package api exposes the engine over a user-only Unix domain socket using
// newline-delimited JSON.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"orbsleep/internal/engine"
	"orbsleep/internal/model"
)

// Version is the protocol version.
const Version = 1

// ErrAlreadyRunning indicates that another daemon owns the socket.
var ErrAlreadyRunning = errors.New("daemon already running")

// Server serves the local API.
type Server struct {
	eng       *engine.Engine
	ln        net.Listener
	path      string
	closing   chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	connMu    sync.Mutex
	conns     map[net.Conn]struct{}

	// baseCtx is canceled by Close so in-flight request handlers stop doing
	// work instead of running to completion (or their own 10-minute timeout)
	// while the process is shutting down.
	baseCtx     context.Context
	baseCancel  context.CancelFunc
	baseCtxOnce sync.Once
}

type request struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type apiError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

type response struct {
	V      int       `json:"v"`
	ID     string    `json:"id"`
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *apiError `json:"error,omitempty"`
}

// New creates the socket server. It refuses to start if another daemon owns
// the socket, and removes a stale socket otherwise.
func New(path string, eng *engine.Engine) (*Server, error) {
	if conn, err := net.DialTimeout("unix", path, 400*time.Millisecond); err == nil {
		conn.Close()
		return nil, fmt.Errorf("%w: 已有 OrbSleep daemon 在运行（%s）", ErrAlreadyRunning, path)
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s 已存在且不是 socket 文件", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{eng: eng, ln: ln, path: path, closing: make(chan struct{}), conns: map[net.Conn]struct{}{}}, nil
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Serve accepts connections until Close.
func (s *Server) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closing:
				return nil
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		s.connMu.Lock()
		s.conns[conn] = struct{}{}
		s.connMu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
			s.connMu.Lock()
			delete(s.conns, conn)
			s.connMu.Unlock()
		}()
	}
}

// Close stops the server.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		close(s.closing)
		s.baseCtxOnce.Do(s.baseCancel)
		s.ln.Close()
		// Close idle GUI connections so shutdown cannot hang on a client that
		// holds the socket open without sending requests.
		s.connMu.Lock()
		for conn := range s.conns {
			conn.Close()
		}
		s.connMu.Unlock()
		s.wg.Wait()
		os.Remove(s.path)
	})
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	var writeMu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.write(&writeMu, conn, response{V: Version, OK: false, Error: &apiError{Code: "invalid_request", Message: "请求不是合法 JSON"}})
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(req request) {
			defer wg.Done()
			defer func() { <-sem }()
			resp := s.dispatch(req)
			s.write(&writeMu, conn, resp)
		}(req)
	}
	wg.Wait()
}

func (s *Server) write(mu *sync.Mutex, conn net.Conn, resp response) {
	raw, err := json.Marshal(resp)
	if err != nil {
		raw = []byte(`{"v":1,"id":"","ok":false,"error":{"code":"internal","message":"encode failed"}}`)
	}
	raw = append(raw, '\n')
	mu.Lock()
	defer mu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	conn.Write(raw)
}

type handler func(ctx context.Context, params json.RawMessage) (any, error)

func (s *Server) dispatch(req request) response {
	resp := response{V: Version, ID: req.ID}
	if req.V != 0 && req.V != Version {
		resp.Error = &apiError{Code: "invalid_request", Message: fmt.Sprintf("不支持的协议版本 %d", req.V)}
		return resp
	}
	h, ok := s.handlers()[req.Method]
	if !ok {
		resp.Error = &apiError{Code: "invalid_request", Message: "未知方法: " + req.Method}
		return resp
	}
	s.baseCtxOnce.Do(func() {
		s.baseCtx, s.baseCancel = context.WithCancel(context.Background())
	})
	ctx, cancel := context.WithTimeout(s.baseCtx, 10*time.Minute)
	defer cancel()
	result, err := h(ctx, req.Params)
	if err != nil {
		var re *apiError
		if errors.As(err, &re) {
			resp.Error = re
		} else {
			resp.Error = &apiError{Code: "internal", Message: err.Error()}
		}
		return resp
	}
	resp.OK = true
	resp.Result = result
	return resp
}

func (s *Server) handlers() map[string]handler {
	return map[string]handler{
		"ping": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return map[string]any{"pong": true, "version": s.eng.Version()}, nil
		},
		"system.shutdown": func(ctx context.Context, _ json.RawMessage) (any, error) {
			// Respond first, then let Serve() unwind and main finish the
			// in-flight transactions before the process exits.
			go func() {
				time.Sleep(150 * time.Millisecond)
				s.Close()
			}()
			return map[string]any{"ok": true, "shuttingDown": true}, nil
		},
		"system.status": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.Status(), nil
		},
		"system.doctor": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Verify bool `json:"verify"`
			}
			if len(params) > 0 {
				if err := decode(params, &p); err != nil {
					return nil, err
				}
			}
			checks := s.eng.Doctor()
			if p.Verify {
				checks = append(checks, s.eng.DoctorVerify()...)
			}
			return map[string]any{"checks": checks}, nil
		},
		"system.recover": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Transaction string `json:"transaction"`
			}
			if len(params) > 0 {
				if err := decode(params, &p); err != nil {
					return nil, err
				}
			}
			if p.Transaction != "" {
				if err := s.eng.RecoverTransaction(p.Transaction); err != nil {
					return nil, err
				}
				return map[string]any{"recovered": true, "transaction": p.Transaction}, nil
			}
			s.eng.RecoverTransactions()
			return map[string]any{"recovered": true}, nil
		},
		"system.health": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.Health(), nil
		},
		"system.prepareShutdown": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.PrepareShutdown(), nil
		},
		"system.prepareUpdate": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.PrepareUpdate(), nil
		},
		"system.recovery.list": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.RecoveryList(), nil
		},
		"system.recovery.resolve": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				ID string `json:"id"`
			}
			if len(params) > 0 {
				_ = json.Unmarshal(params, &p)
			}
			return s.eng.RecoveryResolve(p.ID), nil
		},
		"docker.contexts": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.DockerContexts(ctx), nil
		},
		"docker.context.select": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Name     string `json:"name"`
				Endpoint string `json:"endpoint"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			target := p.Name
			if target == "" {
				target = p.Endpoint
			}
			if target == "" {
				return nil, badRequest("缺少 name 或 endpoint")
			}
			return s.eng.SelectDockerContext(ctx, target)
		},
		"backup.create": func(ctx context.Context, _ json.RawMessage) (any, error) {
			entry, err := s.eng.CreateBackup()
			if err != nil {
				return nil, err
			}
			return map[string]any{"backup": entry}, nil
		},
		"backup.list": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.ListBackups(), nil
		},
		"backup.restore": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				ID   string `json:"id"`
				Path string `json:"path"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			target := p.ID
			if target == "" {
				target = p.Path
			}
			if target == "" {
				return nil, badRequest("缺少备份 id 或 path")
			}
			result, err := s.eng.RestoreBackup(target)
			if err != nil {
				return nil, err
			}
			// Respond first, then stop so the next launch applies the restore.
			go func() {
				time.Sleep(200 * time.Millisecond)
				s.Close()
			}()
			return result, nil
		},
		"logs.export": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Limit int `json:"limit"`
			}
			_ = decode(params, &p)
			return s.eng.ExportLogs(p.Limit)
		},
		"scan": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.Scan(ctx)
		},
		"container.inspect": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				ID string `json:"id"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			if p.ID == "" {
				return nil, badRequest("缺少容器 id")
			}
			cont, raw, err := s.eng.Inspect(ctx, p.ID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"container": cont, "inspect": raw}, nil
		},
		"plan.create": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				ContainerIDs     []string `json:"containerIds"`
				AllowLanExposure bool     `json:"allowLanExposure"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return s.eng.CreatePlan(ctx, p.ContainerIDs, p.AllowLanExposure)
		},
		"plan.apply": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				PlanID string `json:"planId"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			id, err := s.eng.ApplyPlan(p.PlanID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"operationId": id}, nil
		},
		"operation.get": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				ID string `json:"id"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			op, ok, err := s.eng.Operation(p.ID)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, &apiError{Code: "not_found", Message: "操作不存在"}
			}
			return op, nil
		},
		"operations.list": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Limit int `json:"limit"`
			}
			_ = decode(params, &p)
			ops, err := s.eng.Operations(p.Limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{"operations": ops}, nil
		},
		"services.list": func(ctx context.Context, _ json.RawMessage) (any, error) {
			svcs, err := s.eng.Services()
			if err != nil {
				return nil, err
			}
			return map[string]any{"services": svcs}, nil
		},
		"service.action": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				ID     string         `json:"id"`
				Action string         `json:"action"`
				Params map[string]any `json:"params"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			if p.ID == "" || p.Action == "" {
				return nil, badRequest("缺少服务 id 或 action")
			}
			return s.eng.ServiceAction(ctx, p.ID, p.Action, p.Params)
		},
		"settings.get": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.Settings(), nil
		},
		"settings.update": func(ctx context.Context, params json.RawMessage) (any, error) {
			var patch map[string]any
			if err := decode(params, &patch); err != nil {
				return nil, err
			}
			return s.eng.UpdateSettings(patch)
		},
		"proxy.stop": func(ctx context.Context, _ json.RawMessage) (any, error) {
			id, err := s.eng.ProxyStop()
			if err != nil {
				return nil, err
			}
			return map[string]any{"operationId": id}, nil
		},
		"proxy.setup": func(ctx context.Context, _ json.RawMessage) (any, error) {
			id, err := s.eng.ProxySetup()
			if err != nil {
				return nil, err
			}
			return map[string]any{"operationId": id}, nil
		},
		"logs.sources": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return map[string]any{"sources": s.eng.LogSources()}, nil
		},
		"logs.tail": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Limit  int    `json:"limit"`
				Source string `json:"source"`
			}
			_ = decode(params, &p)
			source := p.Source
			if source == "" {
				source = "daemon"
			}
			var (
				lines []model.LogEntry
				err   error
			)
			if source == "proxy" {
				lines, err = s.eng.ProxyLogs(ctx, p.Limit)
			} else {
				lines, err = s.eng.Logs(p.Limit)
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"lines": lines, "source": source}, nil
		},
	}
}

func decode(params json.RawMessage, out any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, out); err != nil {
		return badRequest("参数格式错误: " + err.Error())
	}
	return nil
}

func badRequest(msg string) *apiError {
	return &apiError{Code: "invalid_request", Message: msg}
}
