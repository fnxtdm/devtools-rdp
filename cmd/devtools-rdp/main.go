package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"devtools-rdp/internal/bootstrap"
	"devtools-rdp/internal/config"
	"devtools-rdp/internal/rdp"
	"devtools-rdp/internal/tunnel"
)

// registry keeps one tunnel per configured target, so the control endpoint can
// add forwards to a specific machine while everything is running.
type registry struct {
	mu      sync.Mutex
	tunnels map[string]*tunnel.Server
	order   []string
}

func newRegistry() *registry {
	return &registry{tunnels: map[string]*tunnel.Server{}}
}

func (r *registry) add(name string, s *tunnel.Server) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tunnels[name] = s
	r.order = append(r.order, name)
}

func (r *registry) get(name string) *tunnel.Server {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tunnels[name]
}

func (r *registry) snapshot() map[string][]tunnel.Forward {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]tunnel.Forward{}
	for name, tn := range r.tunnels {
		out[name] = tn.Forwards()
	}
	return out
}

func main() {
	var (
		cfgPath     = flag.String("c", "config.json", "config file")
		only        = flag.String("only", "", "comma separated target names to start")
		agentPath   = flag.String("agent", "", "path to devtools-rdp-agent.exe")
		noBootstrap = flag.Bool("no-bootstrap", false, "expect agents that are already running")
		timeout     = flag.Duration("timeout", 150*time.Second, "wait for each remote agent")
		control     = flag.String("control", "127.0.0.1:9221", "control endpoint for hot-adding forwards (empty to disable)")
		runCmd      = flag.String("run", "", "type one PowerShell command into the target desktop, then exit")
	)
	var forwards multiFlag
	flag.Var(&forwards, "forward", "target:localPort[:remotePort[:remoteHost]] (repeatable)")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Fatalf("%v", err)
	}

	targets := cfg.Targets
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		var picked []config.Target
		for _, t := range targets {
			if want[t.Name] {
				picked = append(picked, t)
			}
		}
		if len(picked) == 0 {
			logger.Fatalf("-only %q matched no target", *only)
		}
		targets = picked
	}

	if *runCmd != "" {
		if len(targets) != 1 {
			logger.Fatalf("-run needs exactly one target (use -only NAME)")
		}
		if err := runOnDesktop(targets[0], *runCmd, logger); err != nil {
			logger.Fatalf("%v", err)
		}
		return
	}

	extra, err := parseForwards(forwards, targets)
	if err != nil {
		logger.Fatalf("%v", err)
	}

	var agentBytes []byte
	if !*noBootstrap {
		agentBytes, err = loadAgent(*agentPath)
		if err != nil {
			logger.Fatalf("agent binary: %v", err)
		}
	}

	reg := newRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t config.Target) {
			defer wg.Done()
			if err := runTarget(ctx, t, extra[t.Name], agentBytes, *noBootstrap, logger, reg); err != nil {
				logger.Printf("[%s] FAILED: %v", t.Name, err)
			}
		}(t)
	}

	if *control != "" {
		go serveControl(*control, reg, logger)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	for _, t := range targets {
		if tn := reg.get(t.Name); tn != nil {
			tn.Close()
		}
	}
}

// runOnDesktop logs in over RDP, opens PowerShell in the interactive desktop and
// types one command. It lets you start something on the remote host (a browser
// with a CDP port, for example) without involving the tunnel agent.
func runOnDesktop(t config.Target, command string, logger *log.Logger) error {
	log := log.New(logger.Writer(), fmt.Sprintf("[%s] ", t.Name), log.LstdFlags)
	client := rdp.New(t.RDP, t.ChannelName, log)
	log.Printf("connecting to %s:%d as %s", t.RDP.Host, t.RDP.Port, t.RDP.Username)
	if err := client.Connect(); err != nil {
		return fmt.Errorf("RDP connect: %w", err)
	}
	log.Printf("waiting for the remote desktop")
	time.Sleep(10 * time.Second)

	client.Hotkey([]string{"lwin"}, "r")
	time.Sleep(1800 * time.Millisecond)
	client.Hotkey([]string{"ctrl"}, "a")
	time.Sleep(200 * time.Millisecond)
	client.TypeText("powershell", 40*time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	client.Tap("enter")
	time.Sleep(6 * time.Second)
	client.Hotkey([]string{"ctrl"}, "c")
	time.Sleep(400 * time.Millisecond)
	client.Tap("enter")
	time.Sleep(400 * time.Millisecond)

	log.Printf("typing %d characters", len(command))
	client.TypeText(command, 20*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	client.Tap("enter")
	time.Sleep(4 * time.Second)
	log.Printf("command sent; disconnecting")
	return nil
}

func runTarget(ctx context.Context, t config.Target, extra []tunnel.Forward, agentBytes []byte,
	noBootstrap bool, logger *log.Logger, reg *registry) error {

	log := log.New(logger.Writer(), fmt.Sprintf("[%s] ", t.Name), log.LstdFlags)

	client := rdp.New(t.RDP, t.ChannelName, log)
	log.Printf("connecting to %s:%d as %s", t.RDP.Host, t.RDP.Port, t.RDP.Username)
	if err := client.Connect(); err != nil {
		return fmt.Errorf("RDP connect: %w", err)
	}
	log.Printf("RDP connected")

	tun := tunnel.New(client.SendChannel, log)
	for _, f := range t.Forwards {
		tun.AddForward(fmt.Sprintf("%s:%d", t.ListenHost, f.LocalPort),
			fmt.Sprintf("%s:%d", remoteHost(f, t.RemoteHost), f.RemotePort))
	}
	for _, f := range extra {
		tun.AddForward(f.Listen, f.Remote)
	}
	if err := tun.Start(); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	reg.add(t.Name, tun)

	var boot *bootstrap.Bootstrapper
	if noBootstrap {
		parser := &tunnel.Parser{}
		client.SetChannelHandler(func(data []byte) {
			frames, err := parser.Feed(data)
			if err != nil {
				log.Printf("frame error: %v", err)
				return
			}
			for _, f := range frames {
				tun.HandleFrame(f)
			}
		})
	} else {
		boot = bootstrap.New(client, tun, t, agentBytes, log)
		if err := boot.Run(ctx); err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
	}

	hello, err := tun.WaitHello(ctx)
	if err != nil {
		return fmt.Errorf("%v (check %%TEMP%%\\devtools-rdp-agent.log on the remote)", err)
	}
	log.Printf("agent: %s", string(hello))
	for _, f := range tun.Forwards() {
		log.Printf("ready: http://%s -> remote %s", f.Listen, f.Remote)
	}
	return nil
}

func remoteHost(f config.Forward, fallback string) string {
	if f.RemoteHost != "" {
		return f.RemoteHost
	}
	return fallback
}

// parseForwards turns -forward flags into per-target mapping lists.
// Format: target:localPort[:remotePort[:remoteHost]]. With a single target the
// name may be omitted.
func parseForwards(flags []string, targets []config.Target) (map[string][]tunnel.Forward, error) {
	byName := map[string]config.Target{}
	for _, t := range targets {
		byName[t.Name] = t
	}
	out := map[string][]tunnel.Forward{}
	for _, spec := range flags {
		parts := strings.Split(spec, ":")
		name := ""
		if len(parts) > 0 {
			if _, ok := byName[parts[0]]; ok {
				name = parts[0]
				parts = parts[1:]
			}
		}
		if name == "" {
			if len(targets) != 1 {
				return nil, fmt.Errorf("-forward %q: prefix the target name (target:localPort[:remotePort[:remoteHost]])", spec)
			}
			name = targets[0].Name
		}
		t := byName[name]
		localPort, remote, err := parseForward(strings.Join(parts, ":"), t.RemoteHost)
		if err != nil {
			return nil, fmt.Errorf("-forward %q: %w", spec, err)
		}
		out[name] = append(out[name], tunnel.Forward{
			Listen: fmt.Sprintf("%s:%d", t.ListenHost, localPort),
			Remote: remote,
		})
	}
	return out, nil
}

func parseForward(spec, defaultHost string) (int, string, error) {
	parts := strings.Split(spec, ":")
	localPort, err := strconv.Atoi(parts[0])
	if err != nil || localPort <= 0 || localPort > 65535 {
		return 0, "", fmt.Errorf("bad local port %q", parts[0])
	}
	host, port := defaultHost, localPort
	switch len(parts) {
	case 1:
	case 2:
		if port, err = strconv.Atoi(parts[1]); err != nil || port <= 0 || port > 65535 {
			return 0, "", fmt.Errorf("bad remote port %q", parts[1])
		}
	case 3:
		host = parts[1]
		if port, err = strconv.Atoi(parts[2]); err != nil || port <= 0 || port > 65535 {
			return 0, "", fmt.Errorf("bad remote port %q", parts[2])
		}
	default:
		return 0, "", fmt.Errorf("want localPort[:remotePort[:remoteHost]]")
	}
	return localPort, fmt.Sprintf("%s:%d", host, port), nil
}

// serveControl exposes the running tunnels over localhost:
//
//	curl -X POST 'http://127.0.0.1:9221/forward?target=cloud&listen=58528'
//	curl -X POST 'http://127.0.0.1:9221/forward?listen=58528'          # single target
//	curl           'http://127.0.0.1:9221/forward'
func serveControl(addr string, reg *registry, logger *log.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/forward", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]interface{}{"targets": reg.snapshot()})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET or POST"})
			return
		}
		name := q.Get("target")
		if name == "" {
			snap := reg.snapshot()
			if len(snap) == 1 {
				for k := range snap {
					name = k
				}
			}
		}
		tun := reg.get(name)
		if tun == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown target " + name})
			return
		}
		localPort, err := strconv.Atoi(q.Get("listen"))
		if err != nil || localPort <= 0 || localPort > 65535 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "listen port is required"})
			return
		}
		remote := q.Get("remote")
		if remote == "" {
			remote = strconv.Itoa(localPort)
		}
		if !strings.Contains(remote, ":") {
			host := q.Get("host")
			if host == "" {
				host = "127.0.0.1"
			}
			remote = host + ":" + remote
		}
		f := tunnel.Forward{Listen: fmt.Sprintf("127.0.0.1:%d", localPort), Remote: remote}
		if err := tun.Listen(f); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "target": name, "forward": f})
	})
	logger.Printf("control endpoint: http://%s/forward", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Printf("control server: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func loadAgent(path string) ([]byte, error) {
	candidates := []string{}
	if path != "" {
		candidates = append(candidates, path)
	}
	exe, _ := os.Executable()
	if exe != "" {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "devtools-rdp-agent.exe"),
			filepath.Join(dir, "..", "dist", "devtools-rdp-agent.exe"),
		)
	}
	candidates = append(candidates, "dist/devtools-rdp-agent.exe", "devtools-rdp-agent.exe")
	for _, c := range candidates {
		if data, err := os.ReadFile(c); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("not found (build it with 'make agent' or pass -agent)")
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}
