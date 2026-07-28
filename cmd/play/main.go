// Command play is the whole remote desktop in ONE process: it runs the signaling
// server, serves the browser client (embedded in the binary), and runs the WebRTC
// host — so starting a session is one command instead of two coordinated terminals.
//
// The only external piece left is the Swift capture helper, and that separation is
// deliberate: it is the TCC boundary that holds the Screen Recording and
// Accessibility grants, and it accepts commands only from this process, never from
// the network (docs/SECURITY.md).
//
// The host talks to the in-process signaling server over loopback WebSocket rather
// than a Go channel. That keeps ONE signaling code path shared with cmd/host, and
// the hop costs nothing: it carries only SDP/ICE at session setup, never media.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"

	play "github.com/yizhifang123/Mac-Web-RemoteControl"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/auth"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/host"
	"github.com/yizhifang123/Mac-Web-RemoteControl/internal/signaling"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "bind address for signaling + client page (KEEP loopback-only)")
	webDir := flag.String("web", "", "serve the client from this directory instead of the embedded copy (dev)")
	configPath := flag.String("config", auth.DefaultConfigPath(), "path to the secrets file (mode 0600)")
	newPassword := flag.Bool("new-password", false, "generate a random password, print it, and exit")
	setPassword := flag.Bool("set-password", false, "read a password from stdin, store it, and exit")
	noAuth := flag.Bool("no-auth", false, "DANGEROUS: serve with no password (loopback binds only)")
	hostConfig := host.BindFlags(flag.CommandLine)
	flag.Parse()

	if *setPassword {
		runSetPassword(*configPath)
		return
	}

	if *webDir != "" {
		log.Printf("serving client from %s (not the embedded copy)", *webDir)
	}

	gate, token := setupAuth(*configPath, *newPassword, *noAuth, *addr)

	// Bind before starting the host so the server is already accepting when it dials.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("bind %s: %v", *addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	log.Printf("signaling + client on http://%s", ln.Addr())
	signaling.LogBindHints(*addr)

	go func() {
		if err := signaling.Serve(ctx, ln, play.WebRoot(*webDir), gate); err != nil && ctx.Err() == nil {
			log.Fatalf("signaling: %v", err)
		}
	}()

	// Always dial loopback: a wildcard bind still has to be reached locally, and this
	// resolves a :0 port to whatever was actually assigned.
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		log.Fatalf("listener address %q: %v", ln.Addr(), err)
	}
	cfg := hostConfig()
	cfg.SignalURL = "ws://127.0.0.1:" + port + "/ws"
	cfg.Token = token

	if err := host.Run(ctx, cfg); err != nil {
		log.Fatalf("host: %v", err)
	}
}

// runSetPassword stores a password read from STDIN. Reading it from stdin rather than
// a flag keeps it out of `ps` output and shell history — a password passed as an
// argument is visible to every process on the machine while the command runs.
func runSetPassword(configPath string) {
	fmt.Fprint(os.Stderr, "New password (input is not hidden; paste or pipe it): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		log.Fatalf("read password: %v", err)
	}
	password := strings.TrimRight(line, "\r\n")

	warning, err := auth.SetPassword(configPath, password)
	if err != nil {
		log.Fatalf("set password: %v", err)
	}
	fmt.Fprintf(os.Stderr, "\npassword updated in %s\n", configPath)
	fmt.Fprintln(os.Stderr, "all existing browser sessions were signed out.")
	if warning != "" {
		fmt.Fprintf(os.Stderr, "note: %s\n", warning)
	}
}

// setupAuth loads (or creates) the gate and returns it with the host's bearer token.
// It refuses the one combination that would expose an ungated input channel to a
// network: -no-auth on anything but a loopback bind.
func setupAuth(configPath string, regenerate, noAuth bool, addr string) (*auth.Auth, string) {
	if noAuth {
		if !isLoopbackBind(addr) {
			log.Fatalf("-no-auth refused: %s is not loopback. This tool injects keystrokes; "+
				"an ungated network bind is remote code execution for anyone who can reach it.", addr)
		}
		log.Printf("!! -no-auth: NO PASSWORD. Loopback only, never through the tunnel.")
		return nil, ""
	}

	gate, password, err := auth.Load(configPath, regenerate)
	if err != nil {
		log.Fatalf("auth: %v", err)
	}
	if password != "" {
		log.Printf("---------------------------------------------------------------")
		log.Printf("  NEW PASSWORD (shown once — save it now):  %s", password)
		log.Printf("  stored as a bcrypt hash in %s", configPath)
		log.Printf("  lost it? rerun with -new-password")
		log.Printf("---------------------------------------------------------------")
		if regenerate {
			os.Exit(0)
		}
	}
	return gate, gate.Token()
}

func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
