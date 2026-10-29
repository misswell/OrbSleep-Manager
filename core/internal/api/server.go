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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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
		"system.status": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.eng.Status(), nil
		},
		"system.doctor": func(ctx context.Context, _ json.RawMessage) (any, error) {
			return map[string]any{"checks": s.eng.Doctor()}, nil
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
		"agent.install": func(ctx context.Context, _ json.RawMessage) (any, error) {
			if err := s.eng.InstallAgent(); err != nil {
				return nil, err
			}
			return map[string]any{"installed": true}, nil
		},
		"agent.uninstall": func(ctx context.Context, _ json.RawMessage) (any, error) {
			if err := s.eng.UninstallAgent(); err != nil {
				return nil, err
			}
			return map[string]any{"installed": false}, nil
		},
		"proxy.setup": func(ctx context.Context, _ json.RawMessage) (any, error) {
			id, err := s.eng.ProxySetup()
			if err != nil {
				return nil, err
			}
			return map[string]any{"operationId": id}, nil
		},
		"logs.tail": func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				Limit int `json:"limit"`
			}
			_ = decode(params, &p)
			lines, err := s.eng.Logs(p.Limit)
			if err != nil {
				return nil, err
			}
			return map[string]any{"lines": lines}, nil
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
