package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func localPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	_, raw, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(raw)
	return port
}

var testClient = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}

func TestBridgeHTTPStreamingAndWebSockets(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			rw.Flush()
			io.Copy(conn, rw)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(201)
		fmt.Fprintf(w, "%s %s %s %s %s", r.Method, r.URL.RequestURI(), body, r.Host, r.Header.Get("Origin"))
		w.(http.Flusher).Flush()
	}))
	defer app.Close()
	port := localPort(t, app)
	bridge, err := newBridge("127.0.0.2", "127.0.0.1", port, testNetworks)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.2:%d/asset?q=1", port), strings.NewReader("hello"))
	req.Header.Set("Origin", "http://lan:4000")
	res, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	want := fmt.Sprintf("POST /asset?q=1 hello localhost:%d http://lan:4000", port)
	if res.StatusCode != 201 || res.Header.Get("X-Upstream") != "yes" || string(body) != want {
		t.Fatalf("%d %s", res.StatusCode, body)
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.2:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(conn, "GET /hot HTTP/1.1\r\nHost: 127.0.0.2:%d\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\nping", port)
	reader := bufio.NewReader(conn)
	status, _ := reader.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatal(status)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "ping" {
		t.Fatalf("upgrade echo: %s %v", echo, err)
	}
	bridge.Close()
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("shutdown left upgraded connection open")
	}
}
