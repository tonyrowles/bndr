package main

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type bridge struct {
	host        string
	server      *http.Server
	listener    net.Listener
	mu          sync.Mutex
	connections map[*bridgeConn]struct{}
	closed      bool
	transport   *http.Transport
}
type bridgeConn struct {
	net.Conn
	owner *bridge
	once  sync.Once
}

func (c *bridgeConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.owner.mu.Lock(); delete(c.owner.connections, c); c.owner.mu.Unlock() })
	return err
}

type bridgeListener struct {
	net.Listener
	owner *bridge
}

func (l bridgeListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &bridgeConn{Conn: conn, owner: l.owner}
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	if l.owner.closed {
		conn.Close()
		return nil, net.ErrClosed
	}
	l.owner.connections[tracked] = struct{}{}
	return tracked, nil
}
func newBridge(lan, host string, port int) (*bridge, error) {
	listener, err := net.Listen("tcp4", net.JoinHostPort(lan, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 90 * time.Second}
	b := &bridge{host: host, listener: listener, connections: make(map[*bridgeConn]struct{}), transport: transport}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(port))}
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(req *httputil.ProxyRequest) { req.SetURL(target); req.Out.Host = "localhost:" + strconv.Itoa(port) },
		Transport: transport, FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "The local app is unavailable.", http.StatusBadGateway)
		},
	}
	b.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			w = upgradeWriter{w}
		}
		proxy.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 10 * time.Second}
	go b.server.Serve(bridgeListener{listener, b})
	return b, nil
}
func (b *bridge) Close() {
	b.mu.Lock()
	b.closed = true
	connections := make([]*bridgeConn, 0, len(b.connections))
	for conn := range b.connections {
		connections = append(connections, conn)
	}
	b.mu.Unlock()
	b.server.Close()
	for _, conn := range connections {
		conn.Close()
	}
	b.transport.CloseIdleConnections()
}

// Preserve bytes already read after an upgrade request's headers. The standard
// reverse proxy copies from the hijacked connection rather than its reader.
type upgradeWriter struct{ http.ResponseWriter }

func (w upgradeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w upgradeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buffer, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	return &bufferedConn{Conn: conn, reader: buffer.Reader}, buffer, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(data []byte) (int, error) { return c.reader.Read(data) }
