package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed public/index.html
var dashboard []byte

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "Print the installed version and exit")
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: bndr [--version]\n\nA LAN dashboard for local development apps. Configure with BNDR_HOST, BNDR_PORT,\nBNDR_EXCLUDE, BNDR_DATA_DIR, and BNDR_CHROMIUM. See README.md for details.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if *showVersion {
		fmt.Printf("bndr %s\n", version)
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	addresses, err := lanAddresses()
	if err != nil {
		return err
	}
	lan := os.Getenv("BNDR_HOST")
	if lan == "" && len(addresses) > 0 {
		lan = addresses[0]
	}
	if lan == "" || !slices.Contains(addresses, lan) {
		return fmt.Errorf("set BNDR_HOST to an IPv4 address assigned to your LAN interface")
	}
	port := 4000
	if raw := os.Getenv("BNDR_PORT"); raw != "" {
		port, err = strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid BNDR_PORT")
		}
	}
	excluded := make(map[int]bool)
	for _, value := range strings.Split(os.Getenv("BNDR_EXCLUDE"), ",") {
		if strings.TrimSpace(value) == "" {
			continue
		}
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("invalid BNDR_EXCLUDE port %q", value)
		}
		excluded[number] = true
	}
	prefs, err := loadPreferences(defaultDataDir())
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(lan, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app := newApplication(ctx, lan, port, excluded, prefs)
	server := dashboardServer(app)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("bndr → http://%s:%d", lan, port)
	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		app.scan(ctx)
		timer := time.NewTicker(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				app.scan(ctx)
			}
		}
	}()
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		cancel()
	}
	server.Close()
	<-scanDone
	app.Close()
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
