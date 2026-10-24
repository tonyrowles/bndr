package main

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Only accept the configured IP authority. Checking Origin against an arbitrary
// Host alone does not stop a hostname that has been rebound to the LAN address.
func validHost(host, lan string, port int) bool {
	return host == net.JoinHostPort(lan, strconv.Itoa(port)) || (port == 80 && host == lan)
}

var dashboardContentPolicy = contentPolicy(dashboard)

func contentPolicy(html []byte) string {
	hashes := func(tag string) string {
		pattern := regexp.MustCompile(`(?s)<` + tag + `(?:\s[^>]*)?>(.*?)</` + tag + `>`)
		var values []string
		for _, match := range pattern.FindAllSubmatch(html, -1) {
			sum := sha256.Sum256(match[1])
			values = append(values, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		return strings.Join(values, " ")
	}
	return "default-src 'none'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; img-src 'self'; connect-src 'self'; script-src " + hashes("script") + "; style-src " + hashes("style")
}
func dashboardHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", dashboardContentPolicy)
}
func dashboardServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
}

// boundNetworks uses only address assignments matching the selected bind IP.
// Other interfaces (including VPNs) do not automatically widen access.
func boundNetworks(lan string) ([]netip.Prefix, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var addresses []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		assigned, err := iface.Addrs()
		if err != nil {
			return nil, fmt.Errorf("read interface %s addresses: %w", iface.Name, err)
		}
		for _, address := range assigned {
			addresses = append(addresses, address.String())
		}
	}
	return networksForAddress(lan, addresses)
}

func networksForAddress(lan string, addresses []string) ([]netip.Prefix, error) {
	ip, err := netip.ParseAddr(lan)
	if err != nil {
		return nil, fmt.Errorf("invalid bind IP: %w", err)
	}
	var result []netip.Prefix
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address)
		if err == nil && prefix.Addr() == ip {
			result = append(result, prefix.Masked())
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("cannot determine allowed subnet for bind IP %s", lan)
	}
	return result, nil
}

func allowedPeer(remote string, networks []netip.Prefix) bool {
	peer, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	ip := peer.Addr().Unmap()
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
