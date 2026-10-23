package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type socket struct {
	Host string
	Port int
}

func lanAddresses() ([]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil {
				result = append(result, ip.String())
			}
		}
	}
	return result, nil
}

func parseListeners(source string, ipv6 bool) []socket {
	var result []socket
	lines := strings.Split(strings.TrimSpace(source), "\n")
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" {
			continue
		}
		address, portHex, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(portHex, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		var host string
		if ipv6 {
			switch address {
			case "00000000000000000000000000000000":
				host = "::"
			case "00000000000000000000000001000000":
				host = "::1"
			default:
				continue
			}
		} else {
			raw, err := hex.DecodeString(address)
			if err != nil || len(raw) != 4 {
				continue
			}
			host = net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
		}
		result = append(result, socket{host, int(port)})
	}
	return result
}

func listeners() ([]socket, error) {
	var result []socket
	for i, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if i == 1 && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, parseListeners(string(data), i == 1)...)
	}
	return result, nil
}

var titlePattern = regexp.MustCompile(`(?is)<title\b[^>]*>(.*?)</title\s*>`)

func cleanTitle(title string) string {
	title = strings.Join(strings.Fields(html.UnescapeString(title)), " ")
	runes := []rune(title)
	if len(runes) > 120 {
		runes = runes[:120]
	}
	return string(runes)
}

type metadata struct {
	Title string
	HTML  bool
}

var probeClient = &http.Client{
	Timeout:       time.Second,
	Transport:     &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func probe(ctx context.Context, host string, port int) (metadata, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/", nil)
	if err != nil {
		return metadata{}, false
	}
	req.Host = fmt.Sprintf("localhost:%d", port)
	req.Header.Set("Accept-Encoding", "identity")
	response, err := probeClient.Do(req)
	if err != nil {
		return metadata{}, false
	}
	defer response.Body.Close()
	result := metadata{HTML: strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "html")}
	// Stop at the title rather than waiting for an entire streaming document.
	var body strings.Builder
	buffer := make([]byte, 4096)
	for body.Len() < 65536 {
		n, err := response.Body.Read(buffer[:min(len(buffer), 65536-body.Len())])
		body.Write(buffer[:n])
		if strings.Contains(strings.ToLower(body.String()), "</title>") || err != nil {
			break
		}
	}
	if match := titlePattern.FindStringSubmatch(body.String()); len(match) > 1 {
		result.Title = cleanTitle(match[1])
		result.HTML = true
	}
	return result, true
}
