package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseListeners(t *testing.T) {
	tests := []struct {
		input string
		ipv6  bool
		want  []socket
	}{
		{"sl local_address rem_address st\n0: 0100007F:223D 00000000:0000 0A\n1: 00000000:0FA0 00000000:0000 01", false, []socket{{"127.0.0.1", 8765}}},
		{"header\n0: 00000000000000000000000001000000:223D 0 0A\n1: 00000000000000000000000000000000:0FA0 0 0A", true, []socket{{"::1", 8765}, {"::", 4000}}},
		{"header\nmalformed\n1: XY:GG 0 0A", false, nil},
	}
	for _, tt := range tests {
		if got := parseListeners(tt.input, tt.ipv6); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("got %+v want %+v", got, tt.want)
		}
	}
}
func TestTitleAndProbe(t *testing.T) {
	if got := cleanTitle(" Research &amp; notes &#x1F4DA;\n Home "); got != "Research & notes 📚 Home" {
		t.Fatal(got)
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>Project &amp; Plans</title>")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer fixture.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(fixture.URL, "http://127.0.0.1:"))
	result, ok := probe(context.Background(), "127.0.0.1", port)
	if !ok || !result.HTML || result.Title != "Project & Plans" {
		t.Fatalf("%+v %v", result, ok)
	}
}
