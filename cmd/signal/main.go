// Command signal runs ONLY the signaling server and the client page. cmd/play is
// the normal way to run the remote desktop; this exists for debugging one half in
// isolation, and for pointing a separately-run host at it.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"

	play "github.com/yizhifang123/Mac-Web-RemoteControl"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/auth"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/signaling"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "bind address (KEEP loopback-only)")
	webDir := flag.String("web", "", "serve the client from this directory instead of the embedded copy (dev)")
	configPath := flag.String("config", auth.DefaultConfigPath(), "path to the secrets file (mode 0600)")
	flag.Parse()

	gate, password, err := auth.Load(*configPath, false)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	if password != "" {
		log.Printf("NEW PASSWORD (shown once — save it now): %s", password)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("bind %s: %v", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.Printf("signaling + client on http://%s", ln.Addr())
	signaling.LogBindHints(*addr)

	if err := signaling.Serve(ctx, ln, play.WebRoot(*webDir), gate); err != nil && ctx.Err() == nil {
		log.Fatalf("signaling: %v", err)
	}
}
