// Command host runs ONLY the WebRTC host, dialing a signaling server started
// elsewhere (cmd/signal). cmd/play is the normal way to run the remote desktop;
// this exists for debugging the media/input half on its own.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/auth"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/host"
)

func main() {
	signalURL := flag.String("signal", "ws://127.0.0.1:9000/ws", "signaling websocket URL")
	configPath := flag.String("config", auth.DefaultConfigPath(), "secrets file holding the signaling token")
	hostConfig := host.BindFlags(flag.CommandLine)
	flag.Parse()

	cfg := hostConfig()
	cfg.SignalURL = *signalURL
	// Read the same secrets file the server uses, to authenticate to it.
	if gate, _, err := auth.Load(*configPath, false); err == nil {
		cfg.Token = gate.Token()
	} else {
		log.Printf("no usable %s (%v); connecting without a token (works only with -no-auth)", *configPath, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := host.Run(ctx, cfg); err != nil {
		log.Fatalf("host: %v", err)
	}
}
