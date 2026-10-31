// Package gateway implements the macOS-side TCP listener that forwards the
// original host port to the lazy proxy's published listen port.
package gateway

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Route is one host port -> proxy listener mapping.
type Route struct {
	Key        string
	ServiceID  string
	BindHost   string // "" means all interfaces
	BindPort   int
	TargetHost string
	TargetPort int
	Protocol   string
	Label      string
	CreatedAt  time.Time
}

type route struct {
	Route
	mu     sync.RWMutex
	ln     net.Listener
	active atomic.Int32
	wg     sync.WaitGroup
	once   sync.Once
}

func (r *route) setTarget(host string, port int, label string) {
	r.mu.Lock()
	r.TargetHost = host
	r.TargetPort = port
	r.Label = label
	r.mu.Unlock()
}

func (r *route) target() (string, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.TargetHost, r.TargetPort
}

// serviceID returns the route's owner under the route lock: Ensure updates
// the field, and readers on other locks (gateway-wide) would otherwise race.
func (r *route) serviceID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ServiceID
}

// Gateway owns all host port listeners.
type Gateway struct {
	mu       sync.Mutex
	routes   map[string]*route
	conns    map[net.Conn]*route
	sem      chan struct{}
	logf     func(level, format string, args ...any)
	onActive func(serviceID string, at time.Time)
	closed   bool
}

// New creates a gateway. logf may be nil.
func New(logf func(level, format string, args ...any)) *Gateway {
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	return &Gateway{
		routes: make(map[string]*route),
		conns:  make(map[net.Conn]*route),
		sem:    make(chan struct{}, 1024),
		logf:   logf,
	}
}

// SetActivityHook registers a callback fired when a connection is accepted.
func (g *Gateway) SetActivityHook(fn func(serviceID string, at time.Time)) { g.onActive = fn }

// Ensure creates or updates a route.
func (g *Gateway) Ensure(r Route) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("gateway 已关闭")
	}
	if existing, ok := g.routes[r.Key]; ok {
		existing.mu.Lock()
		existing.ServiceID = r.ServiceID
		existing.BindHost = r.BindHost
		existing.BindPort = r.BindPort
		existing.Protocol = r.Protocol
		if existing.CreatedAt.IsZero() {
			existing.CreatedAt = time.Now()
		}
		existing.mu.Unlock()
		existing.setTarget(r.TargetHost, r.TargetPort, r.Label)
		return nil
	}
	addr := net.JoinHostPort(r.BindHost, fmt.Sprintf("%d", r.BindPort))
	ln, err := net.Listen(listenNetwork(r.BindHost), addr)
	if err != nil {
		return fmt.Errorf("无法监听 %s: %w", addr, err)
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	if r.Protocol == "" {
		r.Protocol = "tcp"
	}
	rt := &route{Route: r, ln: ln}
	g.routes[r.Key] = rt
	go g.accept(rt)
	g.logf("info", "监听 %s（服务 %s，转发到 %s:%d）", addr, r.ServiceID, r.TargetHost, r.TargetPort)
	return nil
}

// listenNetwork picks the address family for a listener: an empty host binds
// dual-stack, an IPv6 literal must use tcp6 (tcp4 would fail), and everything
// else is IPv4.
func listenNetwork(bindHost string) string {
	host := strings.TrimSpace(bindHost)
	if host == "" {
		return "tcp"
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.To4() == nil {
		return "tcp6"
	}
	return "tcp4"
}

// Remove closes one listener. Established connections are left to drain.
func (g *Gateway) Remove(key string) {
	g.mu.Lock()
	rt, ok := g.routes[key]
	delete(g.routes, key)
	g.mu.Unlock()
	if ok {
		rt.once.Do(func() { rt.ln.Close() })
		g.logf("info", "停止监听 %s", key)
	}
}

// RemoveService closes every listener of a service.
func (g *Gateway) RemoveService(serviceID string) {
	g.mu.Lock()
	var keys []string
	for k, rt := range g.routes {
		if rt.serviceID() == serviceID {
			keys = append(keys, k)
		}
	}
	g.mu.Unlock()
	for _, k := range keys {
		g.Remove(k)
	}
}

// Matches reports whether the existing route has the same ownership, bind
// address and target as r. A key alone is not enough: the proxy target may
// have changed to a different container IP.
func (g *Gateway) Matches(r Route) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rt, ok := g.routes[r.Key]
	if !ok {
		return false
	}
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	proto := r.Protocol
	if proto == "" {
		proto = "tcp"
	}
	return rt.ServiceID == r.ServiceID && rt.BindHost == r.BindHost &&
		rt.BindPort == r.BindPort && rt.TargetHost == r.TargetHost &&
		rt.TargetPort == r.TargetPort && rt.Protocol == proto
}

// Prune removes routes whose keys are not in keep.
func (g *Gateway) Prune(keep map[string]bool) {
	g.mu.Lock()
	var stale []string
	for k := range g.routes {
		if !keep[k] {
			stale = append(stale, k)
		}
	}
	g.mu.Unlock()
	for _, k := range stale {
		g.Remove(k)
	}
}

// ActiveConns returns the number of live connections for a service.
func (g *Gateway) ActiveConns(serviceID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for _, rt := range g.routes {
		if rt.serviceID() == serviceID {
			total += int(rt.active.Load())
		}
	}
	return total
}

// RouteKeys returns the currently installed route keys.
func (g *Gateway) RouteKeys() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.routes))
	for k := range g.routes {
		out = append(out, k)
	}
	return out
}

// StopAll closes every listener and active connection (daemon shutdown) and
// waits — bounded — for the connection handlers to finish.
func (g *Gateway) StopAll() {
	g.mu.Lock()
	g.closed = true
	routes := make([]*route, 0, len(g.routes))
	for _, rt := range g.routes {
		routes = append(routes, rt)
	}
	g.routes = map[string]*route{}
	conns := make([]net.Conn, 0, len(g.conns))
	for conn := range g.conns {
		conns = append(conns, conn)
	}
	g.conns = map[net.Conn]*route{}
	g.mu.Unlock()
	for _, rt := range routes {
		rt.once.Do(func() { rt.ln.Close() })
	}
	// Active connections hold the daemon's port forwarding; a shutdown that
	// leaves them running keeps client sockets alive after "stop".
	for _, conn := range conns {
		conn.Close()
	}
	done := make(chan struct{})
	go func() {
		for _, rt := range routes {
			rt.wg.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		g.logf("warn", "网关连接处理协程未在 10s 内全部退出")
	}
}

func (g *Gateway) accept(rt *route) {
	for {
		conn, err := rt.ln.Accept()
		if err != nil {
			if isClosed(err) {
				return
			}
			g.logf("warn", "接受连接失败 %s: %v", rt.Key, err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case g.sem <- struct{}{}:
		default:
			g.logf("warn", "并发连接达到上限，拒绝 %s", conn.RemoteAddr())
			conn.Close()
			continue
		}
		// Register the connection and count its handler under the gateway
		// lock, re-checking closed: StopAll collects and closes the registry
		// under the same lock, so nothing can register or survive after it.
		g.mu.Lock()
		if g.closed {
			g.mu.Unlock()
			conn.Close()
			return
		}
		g.conns[conn] = rt
		rt.wg.Add(1)
		g.mu.Unlock()
		go func() {
			defer func() {
				g.mu.Lock()
				delete(g.conns, conn)
				g.mu.Unlock()
				<-g.sem
				rt.wg.Done()
			}()
			g.handle(rt, conn)
		}()
	}
}

func (g *Gateway) handle(rt *route, client net.Conn) {
	defer client.Close()
	if tc, ok := client.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}
	rt.active.Add(1)
	defer rt.active.Add(-1)
	if g.onActive != nil {
		g.onActive(rt.serviceID(), time.Now())
	}

	targetHost, targetPort := rt.target()
	target := net.JoinHostPort(targetHost, fmt.Sprintf("%d", targetPort))
	var upstream net.Conn
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		d := net.Dialer{Timeout: 10 * time.Second}
		upstream, err = d.Dial("tcp", target)
		if err == nil {
			break
		}
		g.logf("warn", "连接上游 %s 失败（第 %d 次）: %v", target, attempt+1, err)
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		g.logf("error", "放弃转发 %s -> %s: %v", client.RemoteAddr(), target, err)
		return
	}
	defer upstream.Close()
	if tc, ok := upstream.(*net.TCPConn); ok {
		tc.SetKeepAlive(true)
		tc.SetKeepAlivePeriod(30 * time.Second)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
	// Give the peer a moment to observe the half-close before tearing down.
	time.Sleep(10 * time.Millisecond)
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
		return
	}
	c.Close()
}

func isClosed(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "use of closed network connection") || strings.Contains(msg, "closed network connection")
}
