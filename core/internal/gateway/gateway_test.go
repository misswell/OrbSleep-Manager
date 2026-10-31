package gateway

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func echoServer(t *testing.T) (port int, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { ln.Close(); wg.Wait() }
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return p
}

func TestForwardsTraffic(t *testing.T) {
	targetPort, closeSrv := echoServer(t)
	defer closeSrv()
	bindPort := freePort(t)
	gw := New(nil)
	defer gw.StopAll()
	var activity int32
	gw.SetActivityHook(func(string, time.Time) { atomic.AddInt32(&activity, 1) })
	if err := gw.Ensure(Route{Key: "k", ServiceID: "svc", BindHost: "127.0.0.1", BindPort: bindPort,
		TargetHost: "127.0.0.1", TargetPort: targetPort}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(bindPort))
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "hello" {
		t.Fatalf("echo = %q", buf)
	}
	if got := gw.ActiveConns("svc"); got != 1 {
		t.Fatalf("active = %d", got)
	}
	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for gw.ActiveConns("svc") != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := gw.ActiveConns("svc"); got != 0 {
		t.Fatalf("active after close = %d", got)
	}
	if atomic.LoadInt32(&activity) != 1 {
		t.Fatalf("activity hook = %d", activity)
	}
}

func TestConcurrentClients(t *testing.T) {
	targetPort, closeSrv := echoServer(t)
	defer closeSrv()
	bindPort := freePort(t)
	gw := New(nil)
	defer gw.StopAll()
	if err := gw.Ensure(Route{Key: "k", ServiceID: "svc", BindHost: "127.0.0.1", BindPort: bindPort,
		TargetHost: "127.0.0.1", TargetPort: targetPort}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(bindPort))
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			payload := []byte("ping")
			if _, err := conn.Write(payload); err != nil {
				errs <- err
				return
			}
			buf := make([]byte, 4)
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- err
				return
			}
			if string(buf) != "ping" {
				errs <- io.ErrUnexpectedEOF
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestUpdateTarget(t *testing.T) {
	port1, close1 := echoServer(t)
	defer close1()
	port2, close2 := echoServer(t)
	defer close2()
	bindPort := freePort(t)
	gw := New(nil)
	defer gw.StopAll()
	route := Route{Key: "k", ServiceID: "svc", BindHost: "127.0.0.1", BindPort: bindPort,
		TargetHost: "127.0.0.1", TargetPort: port1}
	if err := gw.Ensure(route); err != nil {
		t.Fatal(err)
	}
	route.TargetPort = port2
	if err := gw.Ensure(route); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(bindPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
}

func TestPortConflictReported(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	gw := New(nil)
	defer gw.StopAll()
	if err := gw.Ensure(Route{Key: "k", ServiceID: "svc", BindHost: "127.0.0.1", BindPort: port,
		TargetHost: "127.0.0.1", TargetPort: 1}); err == nil {
		t.Fatal("expected bind conflict")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestMatchesDetectsTargetChange(t *testing.T) {
	port := freePort(t)
	g := New(nil)
	defer g.RemoveService("svc")
	r := Route{Key: "svc|127.0.0.1|port", ServiceID: "svc", BindHost: "127.0.0.1", BindPort: port, TargetHost: "172.18.0.2", TargetPort: 41000}
	if err := g.Ensure(r); err != nil {
		t.Fatal(err)
	}
	if !g.Matches(r) {
		t.Fatal("route should match")
	}
	changed := r
	changed.TargetHost = "172.18.0.3"
	if g.Matches(changed) {
		t.Fatal("route with changed target must not match")
	}
	changed = r
	changed.ServiceID = "other"
	if g.Matches(changed) {
		t.Fatal("route with changed owner must not match")
	}
}

// StopAll must close established connections (they hold the daemon's port
// forwarding) and wait for the handlers, not just close the listeners.
func TestStopAllClosesActiveConnections(t *testing.T) {
	targetPort, closeSrv := echoServer(t)
	defer closeSrv()
	bindPort := freePort(t)
	gw := New(nil)
	if err := gw.Ensure(Route{Key: "k", ServiceID: "svc", BindHost: "127.0.0.1", BindPort: bindPort,
		TargetHost: "127.0.0.1", TargetPort: targetPort}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(bindPort))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for gw.ActiveConns("svc") != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	gw.StopAll()

	// The client connection must be closed by StopAll: a read returns EOF or
	// an error promptly instead of hanging until the echo server times out.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("connection survived StopAll")
	}
}

// An IPv6 bind address must be served: the listener previously hard-coded
// tcp4 and failed for ::1 bindings.
func TestEnsureListensOnIPv6(t *testing.T) {
	targetPort, closeSrv := echoServer(t)
	defer closeSrv()
	bindPort := freePort(t)
	gw := New(nil)
	defer gw.StopAll()
	if err := gw.Ensure(Route{Key: "k6", ServiceID: "svc6", BindHost: "::1", BindPort: bindPort,
		TargetHost: "127.0.0.1", TargetPort: targetPort}); err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp6", "[::1]:"+itoa(bindPort))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("v6")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "v6" {
		t.Fatalf("echo = %q", buf)
	}
}
