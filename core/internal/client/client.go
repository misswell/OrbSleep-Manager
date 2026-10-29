// Package client is a minimal NDJSON-over-Unix-socket client used by the CLI.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client serialises calls over one socket connection.
type Client struct {
	socket string
	mu     sync.Mutex
	conn   net.Conn
	rd     *bufio.Reader
}

type request struct {
	V      int    `json:"v"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type response struct {
	V      int             `json:"v"`
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

var callSeq int64

// Dial connects to the daemon socket.
func Dial(socket string) (*Client, error) {
	c := &Client{socket: socket}
	if err := c.connect(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) connect() error {
	conn, err := net.DialTimeout("unix", c.socket, 2*time.Second)
	if err != nil {
		return fmt.Errorf("无法连接 OrbSleep 后台助手（%s）: %w", c.socket, err)
	}
	c.conn = conn
	c.rd = bufio.NewReaderSize(conn, 64*1024)
	return nil
}

// Close closes the connection.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Call invokes a method and decodes the result into out.
func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		if err := c.connect(); err != nil {
			return err
		}
	}
	callSeq++
	id := fmt.Sprintf("cli-%d", callSeq)
	payload, err := json.Marshal(request{V: 1, ID: id, Method: method, Params: params})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Minute)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.conn.SetDeadline(deadline)
	if _, err := c.conn.Write(append(payload, '\n')); err != nil {
		c.conn.Close()
		c.conn = nil
		return err
	}
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		c.conn.Close()
		c.conn = nil
		return err
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("daemon 返回了非法响应: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	if !resp.OK {
		return errors.New("daemon 返回失败但未提供错误信息")
	}
	if out != nil {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("解析结果失败: %w", err)
		}
	}
	return nil
}
