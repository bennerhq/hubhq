package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	htmlpkg "html"
	"io"
	"math"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jbenner/hubhq/internal/airplay"
	"github.com/jbenner/hubhq/internal/autos"
	castbackend "github.com/jbenner/hubhq/internal/cast"
	"github.com/jbenner/hubhq/internal/client"
	"github.com/jbenner/hubhq/internal/config"
	"github.com/jbenner/hubhq/internal/discovery"
	"github.com/jbenner/hubhq/internal/dukaone"
	"github.com/jbenner/hubhq/internal/llm"
	"github.com/jbenner/hubhq/internal/output"
	"github.com/jbenner/hubhq/internal/script"
	"github.com/jbenner/hubhq/internal/version"
	"golang.org/x/sys/unix"
)

const (
	audioClipOpened = "ikea://notify/03_device_open"
	audioClipClosed = "ikea://notify/04_device_close"
	startupLogo     = "______        ______ ______\n___  /_____  ____  /____  /_______ _\n__  __ \\  / / /_  __ \\_  __ \\  __ `/\n_  / / / /_/ /_  /_/ /  / / / /_/ /\n/_/ /_/\\__,_/ /_.___//_/ /_/\\__, /\n                              /_/"
)

var inventoryRefreshState struct {
	mu      sync.Mutex
	running bool
}

type command struct {
	json       bool
	yes        bool
	debug      bool
	showScript bool
	noLogo     bool
	stdin      io.Reader
	host       string
	statusPort string
	useHub     bool
	stderr     io.Writer
}

type runtimeWatchedFile struct {
	label          string
	path           string
	restartProcess bool
}

type runtimeStatusEntry struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

type runtimeStatusServer struct {
	mu        sync.Mutex
	mode      string
	startedAt time.Time
	url       string
	summary   map[string]any
	recent    []runtimeStatusEntry
	server    *http.Server
	listener  net.Listener
}

type runtimeInstanceInfo struct {
	ID        string    `json:"id"`
	Mode      string    `json:"mode"`
	PID       int       `json:"pid"`
	Host      string    `json:"host"`
	StartedAt time.Time `json:"started_at"`
	HubID     string    `json:"hub_id,omitempty"`
	HubName   string    `json:"hub_name,omitempty"`
	HubHost   string    `json:"hub_host,omitempty"`
	HubKey    string    `json:"hub_key,omitempty"`
	StatusURL string    `json:"status_url,omitempty"`
	Command   string    `json:"command,omitempty"`
	Args      []string  `json:"args,omitempty"`
}

func autoRunLogf(stdout io.Writer, format string, args ...any) {
	if stdout == nil {
		return
	}
	_, _ = fmt.Fprintf(stdout, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func runtimeLogf(stdout io.Writer, status *runtimeStatusServer, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if status != nil {
		status.Add(message)
	}
	autoRunLogf(stdout, "%s", message)
}

func startRuntimeStatusServer(ctx context.Context, mode, port string) (*runtimeStatusServer, error) {
	port = strings.TrimSpace(port)
	if port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return nil, fmt.Errorf("invalid %s status port %q", mode, port)
		}
	}
	ports := []string{}
	if port == "" {
		for candidate := 8080; candidate <= 8090; candidate++ {
			ports = append(ports, strconv.Itoa(candidate))
		}
	} else {
		ports = append(ports, port)
	}
	advertisedHost := preferredRuntimeStatusHost()
	listenHost := advertisedHost
	if listenHost == "" {
		listenHost = "0.0.0.0"
	}
	var listener net.Listener
	var err error
	var lastErr error
	selectedPort := ""
	for _, candidatePort := range ports {
		listener, err = net.Listen("tcp", net.JoinHostPort(listenHost, candidatePort))
		if err != nil && listenHost != "0.0.0.0" {
			listener, err = net.Listen("tcp", net.JoinHostPort("0.0.0.0", candidatePort))
		}
		if err == nil {
			selectedPort = candidatePort
			break
		}
		lastErr = err
	}
	if listener == nil {
		if lastErr != nil {
			return nil, fmt.Errorf("start %s status server: %w", mode, lastErr)
		}
		return nil, fmt.Errorf("start %s status server: no available status port", mode)
	}
	if selectedPort == "" {
		selectedPort = port
	}
	host, resolvedPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("resolve %s status server address: %w", mode, err)
	}
	if advertisedHost == "" {
		advertisedHost = host
	}
	status := &runtimeStatusServer{
		mode:      mode,
		startedAt: time.Now().UTC(),
		url:       "http://" + net.JoinHostPort(advertisedHost, resolvedPort),
		summary:   map[string]any{},
		listener:  listener,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", status.handleHTML)
	mux.HandleFunc("/status", status.handleJSON)
	mux.HandleFunc("/restart", status.handleRestart)
	server := &http.Server{Handler: mux}
	status.server = server
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		_ = server.Serve(listener)
	}()
	return status, nil
}

func preferredRuntimeStatusHost() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip == nil {
				continue
			}
			if ipv4 := ip.To4(); ipv4 != nil && !ipv4.IsLoopback() {
				return ipv4.String()
			}
		}
	}
	return ""
}

func (s *runtimeStatusServer) URL() string {
	if s == nil {
		return ""
	}
	return s.url
}

func (s *runtimeStatusServer) Add(message string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recent = append(s.recent, runtimeStatusEntry{
		Time:    time.Now().UTC(),
		Message: message,
	})
	if len(s.recent) > 100 {
		s.recent = append([]runtimeStatusEntry(nil), s.recent[len(s.recent)-100:]...)
	}
}

func (s *runtimeStatusServer) SetSummary(summary map[string]any) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if summary == nil {
		s.summary = map[string]any{}
		return
	}
	s.summary = summary
}

func (s *runtimeStatusServer) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := make([]runtimeStatusEntry, len(s.recent))
	copy(recent, s.recent)
	summary := map[string]any{}
	for key, value := range s.summary {
		summary[key] = value
	}
	return map[string]any{
		"mode":       s.mode,
		"started_at": s.startedAt,
		"url":        s.url,
		"summary":    summary,
		"recent":     recent,
	}
}

func (s *runtimeStatusServer) handleJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(s.snapshot())
}

func (s *runtimeStatusServer) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("restarting"))
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = restartCurrentProcess()
	}()
}

func (s *runtimeStatusServer) handleHTML(w http.ResponseWriter, r *http.Request) {
	snapshot := s.snapshot()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, _ := json.MarshalIndent(snapshot, "", "  ")
	escaped := htmlpkg.EscapeString(string(data))
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html>
<head>
  <meta charset="utf-8">
  <title>hubhq %s status</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <style>
    body { font-family: sans-serif; margin: 24px; background: #f7f7f5; color: #222; }
    h1 { margin: 0 0 8px 0; }
    .meta { margin-bottom: 16px; color: #555; }
    .status { padding: 12px 14px; background: #fff; border: 1px solid #ddd; border-radius: 8px; }
    pre { white-space: pre-wrap; word-break: break-word; margin: 0; }
  </style>
</head>
<body>
  <h1>hubhq %s status</h1>
  <div class="meta">Live status page. Refreshes every second.</div>
  <div class="status"><pre id="status">%s</pre></div>
  <script>
    async function refreshStatus() {
      try {
        const response = await fetch('/status', { cache: 'no-store' });
        const data = await response.json();
        document.getElementById('status').textContent = JSON.stringify(data, null, 2);
      } catch (err) {
        document.getElementById('status').textContent = 'status fetch failed: ' + err;
      }
    }
    refreshStatus();
    setInterval(refreshStatus, 1000);
  </script>
</body>
</html>`, s.mode, s.mode, escaped)
}

func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := command{stdin: stdin, stderr: stderr}
	args = consumeBoolFlag(args, "--json", &cmd.json)
	args = consumeBoolFlag(args, "--yes", &cmd.yes)
	args = consumeBoolFlag(args, "--debug", &cmd.debug)
	args = consumeBoolFlag(args, "--show-script", &cmd.showScript)
	args = consumeBoolFlag(args, "--no-logo", &cmd.noLogo)
	args = consumeBoolFlag(args, "--hub", &cmd.useHub)
	args = consumeValueFlag(args, "--host", &cmd.host)
	args = consumeValueFlag(args, "--status-port", &cmd.statusPort)
	if len(args) > 0 && args[0] == "--version" {
		info := version.Current()
		if cmd.json {
			return output.JSON(stdout, map[string]any{
				"version": info.Version,
				"date":    info.Date,
				"time":    info.Time,
			})
		}
		_, err := fmt.Fprintf(stdout, "%s\nDate: %s\nTime: %s\n", info.Version, info.Date, info.Time)
		return err
	}
	printStartupLogo(stdout, cmd.json, cmd.noLogo)
	if len(args) == 0 {
		usage(stdout)
		return nil
	}

	config.SetDebugLogger(cmd.debugf)
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}

	switch args[0] {
	case "help", "--help", "-h":
		usage(stdout)
		return nil
	case "hub":
		return cmd.hub(ctx, cfg, args[1:], stdout)
	case "config":
		return cmd.config(cfg, args[1:], stdout)
	case "access":
		return cmd.access(ctx, cfg, args[1:], stdout)
	case "dukaone":
		return cmd.dukaone(ctx, cfg, args[1:], stdout)
	case "refresh":
		return cmd.refresh(ctx, cfg, stdout)
	case "dump":
		return cmd.dump(ctx, cfg, stdout)
	case "devices":
		return cmd.devices(ctx, cfg, args[1:], stdout)
	case "scenes":
		return cmd.scenes(ctx, cfg, args[1:], stdout)
	case "watch":
		return cmd.watch(ctx, cfg, args[1:], stdout)
	case "speaker":
		return cmd.speaker(ctx, cfg, args[1:], stdout)
	case "ask":
		return cmd.ask(ctx, cfg, args[1:], stdout)
	case "auto":
		return cmd.auto(ctx, cfg, args[1:], stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printStartupLogo(stdout io.Writer, jsonMode, noLogo bool) {
	if jsonMode || noLogo || !writerLooksInteractive(stdout) {
		return
	}
	versionText := version.String()
	width := 0
	for _, line := range strings.Split(startupLogo, "\n") {
		if len(line) > width {
			width = len(line)
		}
	}
	padding := width - len(versionText)
	if padding < 0 {
		padding = 0
	}
	_, _ = fmt.Fprintf(stdout, "\033[93m%s\033[0m\n\033[90m%s%s\033[0m\n\n", startupLogo, strings.Repeat(" ", padding), versionText)
}

func writerLooksInteractive(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok || file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func readScriptStdin(r io.Reader) (string, error) {
	if r == nil {
		return "", nil
	}
	if file, ok := r.(*os.File); ok && file != nil {
		info, err := file.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return "", nil
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(data), nil
}

func (c command) debugf(format string, args ...any) {
	if !c.debug || c.stderr == nil {
		return
	}
	fmt.Fprintf(c.stderr, "[debug] "+format+"\n", args...)
}

func (c command) debugJSON(label string, value any) {
	if !c.debug || c.stderr == nil {
		return
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		c.debugf("%s: %v", label, err)
		return
	}
	fmt.Fprintf(c.stderr, "[debug] %s:\n%s\n", label, data)
}

func (c command) debugScript(label, source string) {
	if !c.debug || c.stderr == nil {
		return
	}
	if strings.TrimSpace(source) == "" {
		fmt.Fprintf(c.stderr, "[debug] %s: <empty>\n", label)
		return
	}
	fmt.Fprintf(c.stderr, "[debug] %s BEGIN\n%s\n[debug] %s END\n", label, source, label)
}

func (c command) debugSpeakerTargets(label string, targets []speakerTarget) {
	if !c.debug || c.stderr == nil {
		return
	}
	view := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		item := map[string]any{
			"id":       target.ID,
			"name":     target.Name,
			"room":     target.Room,
			"kind":     target.Kind,
			"playback": target.Playback,
			"volume":   target.Volume,
		}
		switch target.Kind {
		case "cast":
			item["host"] = target.CastDevice.Host
			item["port"] = target.CastDevice.Port
		case "airplay":
			item["host"] = target.AirPlay.Host
			item["port"] = target.AirPlay.Port
		}
		view = append(view, item)
	}
	c.debugJSON(label, view)
}

func (c command) patchDeviceAttributes(ctx context.Context, cli *client.Client, deviceID string, attrs map[string]any) (map[string]any, error) {
	path := "/v1/devices/" + deviceID
	bodies := []any{
		[]map[string]any{
			{
				"attributes": attrs,
			},
		},
		map[string]any{"attributes": attrs},
		attrs,
	}
	var errs []string
	for i, body := range bodies {
		c.debugf("ask: patch %s attempt=%d", path, i+1)
		c.debugJSON("ask request", body)
		var resp map[string]any
		if err := cli.Patch(ctx, path, body, &resp); err != nil {
			errs = append(errs, err.Error())
			c.debugf("ask: patch attempt=%d failed err=%v", i+1, err)
			continue
		}
		c.debugJSON("ask response", resp)
		return resp, nil
	}
	return nil, errors.New(strings.Join(errs, "; "))
}

func (c command) config(cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: hubhq config show | get <key> | set <key> <value>")
	}
	switch args[0] {
	case "show":
		accessView := cfg.Access
		if strings.TrimSpace(accessView.Path) == "" {
			path, err := config.AccessRoot(cfg)
			if err != nil {
				return err
			}
			accessView.Path = path
		}
		view := map[string]any{
			"default_hub": cfg.DefaultHub,
			"hubs":        cfg.Hubs,
			"llm":         cfg.LLM,
			"apple":       cfg.Apple,
			"dukaone":     cfg.DukaOne,
			"location":    cfg.Location,
			"access":      accessView,
		}
		if c.json {
			return output.JSON(stdout, view)
		}
		return printMap(stdout, view)
	case "get":
		if len(args) < 2 {
			return errors.New("usage: hubhq config get <key>")
		}
		value, err := configGetValue(cfg, args[1])
		if err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{"key": args[1], "value": value})
		}
		_, err = fmt.Fprintln(stdout, value)
		return err
	case "set":
		if len(args) < 3 {
			return errors.New("usage: hubhq config set <key> <value>")
		}
		value := strings.Join(args[2:], " ")
		if err := configSetValue(cfg, args[1], value); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{"key": args[1], "value": value, "status": "saved"})
		}
		_, err := fmt.Fprintf(stdout, "saved %s\n", args[1])
		return err
	default:
		return fmt.Errorf("unknown config command %q", args[0])
	}
}

func (c command) access(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "show" {
		root, err := config.AccessRoot(cfg)
		if err != nil {
			return err
		}
		dirs := map[string]string{}
		builders := []struct {
			key string
			fn  func(*config.Config) (string, error)
		}{
			{key: "data", fn: config.AccessDataDir},
			{key: "html", fn: config.AccessHTMLDir},
			{key: "sounds", fn: config.AccessSoundsDir},
			{key: "scripts", fn: config.AccessScriptsDir},
			{key: "skills", fn: config.AccessSkillsDir},
			{key: "tools", fn: config.AccessToolsDir},
		}
		for _, item := range builders {
			path, err := item.fn(cfg)
			if err != nil {
				return err
			}
			dirs[item.key] = path
		}
		payload := map[string]any{
			"path": root,
			"dirs": dirs,
		}
		if c.json {
			return output.JSON(stdout, payload)
		}
		rows := [][]string{
			{"root", root},
			{"data", dirs["data"]},
			{"html", dirs["html"]},
			{"sounds", dirs["sounds"]},
			{"scripts", dirs["scripts"]},
			{"skills", dirs["skills"]},
			{"tools", dirs["tools"]},
		}
		return output.Table(stdout, []string{"SCOPE", "PATH"}, rows)
	}
	switch args[0] {
	case "init":
		if err := config.EnsureAccessLayout(cfg); err != nil {
			return err
		}
		root, err := config.AccessRoot(cfg)
		if err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{"status": "initialized", "path": root})
		}
		_, err = fmt.Fprintf(stdout, "initialized access layout at %s\n", root)
		return err
	case "run":
		if len(args) < 2 {
			return errors.New("usage: hubhq access run <script> [args...]")
		}
		return c.accessRun(ctx, cfg, args[1], args[2:], stdout)
	default:
		return fmt.Errorf("unknown access command %q", args[0])
	}
}

func (c command) accessRun(ctx context.Context, cfg *config.Config, name string, args []string, stdout io.Writer) error {
	stdinText, err := readScriptStdin(c.stdin)
	if err != nil {
		return err
	}
	var (
		deviceService    script.DeviceService
		cliClient        *client.Client
		homeCtx          llm.HomeContext
		speakerTargets   []speakerTarget
		speakerSnapshots []map[string]any
		speakerService   script.SpeakerService
	)
	if _, err := cfg.DefaultHubConfig(); err == nil {
		if cli, err := hubClient(cfg); err == nil {
			cliClient = cli
			if collected, speakers, err := collectHomeContextWithSpeakers(ctx, cfg); err == nil {
				homeCtx = collected
				speakerTargets = speakers
				speakerSnapshots = speakerSnapshotsFromHomeContext(homeCtx)
			}
			deviceService = runtimeDeviceService{
				cli:              cliClient,
				cfg:              cfg,
				inventoryDevices: homeCtx.Devices,
				inventoryScenes:  homeCtx.Scenes,
				speakerTargets:   speakerTargets,
			}
			speakerService = runtimeSpeakerService{cmd: c, cfg: cfg, targets: speakerTargets}
		}
	}
	stateStore := newRuntimeStateStore()
	httpClient := runtimeHTTPClient{}
	toolService := runtimeToolService{cfg: cfg}
	logger := runtimeLogger{cmd: c}
	accessService := runtimeAccessService{
		cmd:              c,
		cfg:              cfg,
		devices:          deviceService,
		deviceSnapshots:  homeCtx.Devices,
		sceneSnapshots:   homeCtx.Scenes,
		speakers:         speakerService,
		speakerSnapshots: speakerSnapshots,
		state:            stateStore,
		http:             httpClient,
		tools:            toolService,
		logger:           logger,
		now:              time.Now,
		stdinText:        stdinText,
		stdout:           stdout,
		stderr:           c.stderr,
	}
	value, err := accessService.RunScript(ctx, name, args, nil)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, map[string]any{
			"script": name,
			"args":   args,
			"result": value,
		})
	}
	if ok, err := renderAskOutput(stdout, value); ok || err != nil {
		return err
	}
	if text := askResponseText(value); strings.TrimSpace(text) != "" {
		_, err := fmt.Fprintln(stdout, text)
		return err
	}
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		_, err := fmt.Fprintln(stdout, typed)
		return err
	case map[string]any:
		return printMap(stdout, typed)
	default:
		return output.JSON(stdout, value)
	}
}

func (c command) dukaone(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: hubhq dukaone discover [broadcast-address] | list | add | add <device-id> <name> [ip-address] [room] | rename <name-or-id> <new-name> | room <name-or-id> <room> | remove <name-or-id> [--yes]")
	}
	switch args[0] {
	case "discover":
		broadcast := ""
		if len(args) > 1 {
			broadcast = args[1]
		}
		items, err := dukaone.Discover(ctx, cfg, broadcast, 2)
		if err != nil {
			return err
		}
		if configured, configuredErr := dukaone.List(ctx, cfg); configuredErr == nil && len(configured) > 0 {
			configuredByID := map[string]map[string]any{}
			for _, item := range configured {
				configuredByID[stringValue(mapLookup(item, "dukaone", "deviceId"))] = item
				configuredByID[idOf(item)] = item
			}
			for i, item := range items {
				if replacement, ok := configuredByID[stringValue(mapLookup(item, "dukaone", "deviceId"))]; ok {
					items[i] = replacement
					continue
				}
				if replacement, ok := configuredByID[idOf(item)]; ok {
					items[i] = replacement
				}
			}
		}
		if c.json {
			return output.JSON(stdout, items)
		}
		rows := make([][]string, 0, len(items))
		for _, item := range items {
			rows = append(rows, []string{
				stringValue(mapLookup(item, "dukaone", "deviceId")),
				nameOf(item),
				stringValue(mapLookup(item, "attributes", "ipAddress")),
				stringValue(mapLookup(item, "attributes", "humidity")),
				stringValue(mapLookup(item, "attributes", "preset")),
				stringValue(mapLookup(item, "attributes", "mode")),
			})
		}
		return output.Table(stdout, []string{"DEVICE ID", "NAME", "IP", "HUMIDITY", "PRESET", "MODE"}, rows)
	case "list":
		items, err := dukaone.List(ctx, cfg)
		if err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, items)
		}
		headers := []string{
			"NAME",
			"TYPE",
			"ROOM",
			"IP",
			"HUMIDITY",
			"SPEED",
			"PRESET",
			"VENTILATION MODE",
			"MANUAL SPEED",
			"FILTER TIMER",
			"FILTER ALARM",
			"FAN RPM",
			"FIRMWARE VERSION",
			"FIRMWARE DATE",
			"UNIT TYPE",
			"ON",
			"ID",
		}
		rows := make([][]string, 0, len(items))
		for _, item := range items {
			row := []string{
				nameOf(item),
				stringValue(item["type"]),
				nestedString(item, "room", "name"),
				stringValue(mapLookup(item, "attributes", "ipAddress")),
				stringValue(mapLookup(item, "attributes", "humidity")),
				stringValue(mapLookup(item, "attributes", "speed")),
				stringValue(mapLookup(item, "attributes", "preset")),
				stringValue(mapLookup(item, "attributes", "ventilationMode"), mapLookup(item, "attributes", "mode")),
				stringValue(mapLookup(item, "attributes", "manualSpeed")),
				stringValue(mapLookup(item, "attributes", "filterTimer")),
				stringValue(mapLookup(item, "attributes", "filterAlarm")),
				stringValue(mapLookup(item, "attributes", "fanRPM")),
				stringValue(mapLookup(item, "attributes", "firmwareVersion")),
				stringValue(mapLookup(item, "attributes", "firmwareDate")),
				stringValue(mapLookup(item, "attributes", "unitType")),
				stringValue(mapLookup(item, "attributes", "isOn")),
				idOf(item),
			}
			rows = append(rows, row)
		}
		return output.Table(stdout, headers, rows)
	case "add":
		if len(args) == 1 {
			items, err := dukaone.Discover(ctx, cfg, "", 2)
			if err != nil {
				return err
			}
			added := make([]map[string]any, 0, len(items))
			skipped := make([]map[string]any, 0, len(items))
			for _, item := range items {
				deviceID := stringValue(mapLookup(item, "dukaone", "deviceId"))
				name := nameOf(item)
				host := stringValue(mapLookup(item, "attributes", "ipAddress"))
				if deviceID == "" {
					continue
				}
				if dukaone.HasConfig(cfg, deviceID) || dukaone.HasConfig(cfg, dukaone.QualifiedID(deviceID)) {
					skipped = append(skipped, map[string]any{
						"device_id": deviceID,
						"name":      name,
						"host":      host,
						"reason":    "already_configured",
					})
					continue
				}
				if err := dukaone.Add(cfg, deviceID, firstNonEmpty(name, deviceID), host, ""); err != nil {
					return err
				}
				added = append(added, map[string]any{
					"device_id": deviceID,
					"name":      firstNonEmpty(name, deviceID),
					"host":      host,
				})
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			if c.json {
				return output.JSON(stdout, map[string]any{
					"status":  "saved",
					"added":   added,
					"skipped": skipped,
				})
			}
			for _, item := range added {
				fmt.Fprintf(stdout, "Saved DukaOne device %s (%s)\n", item["name"], item["device_id"])
			}
			if len(added) == 0 {
				fmt.Fprintln(stdout, "No new DukaOne devices were added")
			}
			if len(skipped) > 0 {
				fmt.Fprintf(stdout, "Skipped %d already configured DukaOne device(s)\n", len(skipped))
			}
			return nil
		}
		if len(args) < 3 {
			return errors.New("usage: hubhq dukaone add | add <device-id> <name> [ip-address] [room]")
		}
		host := ""
		if len(args) > 3 {
			host = args[3]
		}
		room := ""
		if len(args) > 4 {
			room = strings.Join(args[4:], " ")
		}
		if err := dukaone.Add(cfg, args[1], args[2], host, room); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{
				"status":    "saved",
				"device_id": args[1],
				"name":      args[2],
				"host":      host,
				"room":      room,
			})
		}
		_, err := fmt.Fprintf(stdout, "Saved DukaOne device %s (%s)\n", args[2], args[1])
		return err
	case "remove", "delete":
		if len(args) < 2 {
			return errors.New("usage: hubhq dukaone remove <name-or-id> [--yes]")
		}
		if !c.yes {
			return fmt.Errorf("refusing to remove %s without --yes", args[1])
		}
		if err := dukaone.Delete(cfg, args[1]); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{"status": "removed", "query": args[1]})
		}
		_, err := fmt.Fprintf(stdout, "Removed DukaOne device %s\n", args[1])
		return err
	case "rename":
		if len(args) < 3 {
			return errors.New("usage: hubhq dukaone rename <name-or-id> <new-name>")
		}
		newName := strings.Join(args[2:], " ")
		if err := dukaone.Rename(cfg, args[1], newName); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{"status": "renamed", "query": args[1], "name": newName})
		}
		_, err := fmt.Fprintf(stdout, "Renamed DukaOne device %s to %s\n", args[1], newName)
		return err
	case "room":
		if len(args) < 3 {
			return errors.New("usage: hubhq dukaone room <name-or-id> <room>")
		}
		room := strings.Join(args[2:], " ")
		if err := dukaone.SetRoom(cfg, args[1], room); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{"status": "updated", "query": args[1], "room": room})
		}
		_, err := fmt.Fprintf(stdout, "Set DukaOne device %s room to %s\n", args[1], room)
		return err
	default:
		return fmt.Errorf("unknown dukaone command %q", args[0])
	}
}

func (c command) auth(ctx context.Context, cfg *config.Config, stdin io.Reader, stdout io.Writer) error {
	var hub discovery.Hub
	var err error
	if c.host != "" {
		hub = discovery.Hub{
			ID:   sanitizeHostID(c.host),
			Name: c.host,
			Host: c.host,
			Port: 8443,
		}
	} else {
		hubs, err := discovery.Find(ctx, 5*time.Second)
		if err != nil {
			return fmt.Errorf("%w; try `hubhq hub auth --host <hub-ip>` if mDNS discovery is not working on your network", err)
		}
		hub, err = chooseHub(stdin, stdout, hubs)
		if err != nil {
			return err
		}
	}

	if err := requireReachableHub(ctx, hub.Host); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Using hub %s (%s:%d)\n", hub.Name, hub.Host, hub.Port)
	fmt.Fprintln(stdout, "Delegating pairing to lpgera/dirigera. Follow its prompt and press the hub action button when asked.")

	token, err := authenticateWithDirigeraPackage(ctx, hub.Host, stdin, stdout, c.debug)
	if err != nil {
		return err
	}

	cfg.SetHub(config.Hub{
		ID:           hub.ID,
		Name:         hub.Name,
		Host:         hub.Host,
		Port:         hub.Port,
		Token:        token,
		DiscoveredAt: time.Now().UTC(),
	}, true)
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Stored credentials for %s\n", hub.Name)
	return nil
}

func authenticateWithDirigeraPackage(ctx context.Context, host string, stdin io.Reader, stdout io.Writer, debug bool) (string, error) {
	args := []string{
		"dirigera",
		"authenticate",
		"--gateway-IP", host,
		"--no-reject-unauthorized",
	}
	cmd := exec.CommandContext(ctx, "npx", args...)
	cmd.Stdin = stdin
	var output bytes.Buffer
	cmd.Stdout = io.MultiWriter(stdout, &output)
	cmd.Stderr = io.MultiWriter(stdout, &output)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			}
		case <-done:
		}
	}()
	defer close(done)

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", errors.New("interrupted")
		}
		return "", err
	}

	re := regexp.MustCompile(`(?m)access token:\s*([A-Za-z0-9._~-]+)`)
	match := re.FindStringSubmatch(output.String())
	if len(match) != 2 {
		if debug {
			return "", fmt.Errorf("could not parse access token from lpgera/dirigera output:\n%s", output.String())
		}
		return "", errors.New("could not parse access token from lpgera/dirigera output; rerun with --debug to inspect")
	}
	return match[1], nil
}

func (c command) hub(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) > 0 && args[0] == "probe" {
		if len(args) < 2 {
			return errors.New("usage: hubhq hub probe <host>")
		}
		return probeHub(ctx, args[1], stdout)
	}
	if len(args) > 0 && args[0] == "auth" {
		return c.auth(ctx, cfg, c.stdin, stdout)
	}
	if len(args) > 0 && args[0] == "import-token" {
		if len(args) < 3 {
			return errors.New("usage: hubhq hub import-token <host> <token>")
		}
		return c.hubImportToken(ctx, cfg, args[1], args[2], stdout)
	}
	if len(args) > 0 && args[0] == "discover" {
		discovered, err := discovery.Find(ctx, 5*time.Second)
		if err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, discovered)
		}
		rows := make([][]string, 0, len(discovered))
		for _, hub := range discovered {
			rows = append(rows, []string{hub.Name, hub.Host, strconv.Itoa(hub.Port), hub.ID})
		}
		return output.Table(stdout, []string{"NAME", "HOST", "PORT", "ID"}, rows)
	}
	if len(args) == 0 || args[0] == "list" {
		hubs := cfg.SortedHubs()
		if len(hubs) == 0 {
			discovered, err := discovery.Find(ctx, 3*time.Second)
			if err != nil {
				return fmt.Errorf("%w; try `hubhq hub auth --host <hub-ip>` or `hubhq hub probe <hub-ip>`", err)
			}
			if c.json {
				return output.JSON(stdout, discovered)
			}
			rows := make([][]string, 0, len(discovered))
			for _, hub := range discovered {
				rows = append(rows, []string{hub.Name, hub.Host, strconv.Itoa(hub.Port), hub.ID})
			}
			return output.Table(stdout, []string{"NAME", "HOST", "PORT", "ID"}, rows)
		}
		if c.json {
			return output.JSON(stdout, hubs)
		}
		rows := make([][]string, 0, len(hubs))
		for _, hub := range hubs {
			defaultMarker := ""
			if cfg.DefaultHub == hub.ID {
				defaultMarker = "*"
			}
			rows = append(rows, []string{defaultMarker + hub.Name, hub.Host, strconv.Itoa(hub.Port), hub.ID, "configured"})
		}
		return output.Table(stdout, []string{"NAME", "HOST", "PORT", "ID", "SOURCE"}, rows)
	}
	return fmt.Errorf("unknown hub command %q", args[0])
}

func (c command) hubImportToken(ctx context.Context, cfg *config.Config, host, token string, stdout io.Writer) error {
	if err := requireReachableHub(ctx, host); err != nil {
		return err
	}

	cli := client.New(host, 8443, token)
	var devices []map[string]any
	if err := cli.Get(ctx, "/v1/devices", &devices); err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}

	hubID := sanitizeHostID(host)
	hubName := host
	if discovered, err := discovery.Find(ctx, 3*time.Second); err == nil {
		for _, hub := range discovered {
			if hub.Host == host {
				hubID = hub.ID
				hubName = hub.Name
				break
			}
		}
	}

	cfg.SetHub(config.Hub{
		ID:           hubID,
		Name:         hubName,
		Host:         host,
		Port:         8443,
		Token:        token,
		DiscoveredAt: time.Now().UTC(),
	}, true)
	if err := config.Save(cfg); err != nil {
		return err
	}

	if c.json {
		return output.JSON(stdout, map[string]any{
			"host":    host,
			"id":      hubID,
			"name":    hubName,
			"devices": len(devices),
			"status":  "stored",
		})
	}
	fmt.Fprintf(stdout, "Stored token for %s (%s); validated against %d devices\n", hubName, host, len(devices))
	return nil
}

func (c command) refresh(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	home, err := refreshInventoryCache(ctx, cfg)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, cfg.Cache)
	}
	fmt.Fprintf(stdout, "Cache refreshed: %d devices, %d scenes\n", len(home.Devices), len(home.Scenes))
	return nil
}

func (c command) dump(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	home, err := cli.Home(ctx)
	if err != nil {
		return err
	}
	return output.JSON(stdout, home)
}

func (c command) devices(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("devices command required")
	}
	switch args[0] {
	case "list":
		return c.devicesList(ctx, cfg, stdout)
	case "get":
		if len(args) < 2 {
			return errors.New("usage: hubhq devices get <name-or-id>")
		}
		return c.devicesGet(ctx, cfg, args[1], stdout)
	case "set":
		if len(args) < 3 {
			return errors.New("usage: hubhq devices set <name-or-id> key=value...")
		}
		return c.devicesSet(ctx, cfg, args[1], args[2:], stdout)
	case "rename":
		if len(args) < 3 {
			return errors.New("usage: hubhq devices rename <name-or-id> <new-name>")
		}
		return c.devicesRename(ctx, cfg, args[1], strings.Join(args[2:], " "), stdout)
	case "delete":
		if len(args) < 2 {
			return errors.New("usage: hubhq devices delete <name-or-id> [--yes]")
		}
		return c.devicesDelete(ctx, cfg, args[1], stdout)
	default:
		return fmt.Errorf("unknown devices command %q", args[0])
	}
}

func (c command) scenes(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("scenes command required")
	}
	switch args[0] {
	case "list":
		return c.scenesList(ctx, cfg, stdout)
	case "get":
		if len(args) < 2 {
			return errors.New("usage: hubhq scenes get <name-or-id>")
		}
		return c.scenesGet(ctx, cfg, args[1], stdout)
	case "trigger":
		if len(args) < 2 {
			return errors.New("usage: hubhq scenes trigger <name-or-id>")
		}
		return c.scenesTrigger(ctx, cfg, args[1], stdout)
	case "rename":
		if len(args) < 3 {
			return errors.New("usage: hubhq scenes rename <name-or-id> <new-name>")
		}
		return c.scenesRename(ctx, cfg, args[1], strings.Join(args[2:], " "), stdout)
	case "delete":
		if len(args) < 2 {
			return errors.New("usage: hubhq scenes delete <name-or-id> [--yes]")
		}
		return c.scenesDelete(ctx, cfg, args[1], stdout)
	default:
		return fmt.Errorf("unknown scenes command %q", args[0])
	}
}

func (c command) watch(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "restart":
			return c.restartRuntimeInstances(stdout)
		default:
			return fmt.Errorf("unknown watch command %q", args[0])
		}
	}
	status, err := startRuntimeStatusServer(ctx, "watch", c.statusPort)
	if err == nil {
		runtimeLogf(stdout, status, "watch status available at %s", status.URL())
	} else {
		c.debugf("watch status server: %v", err)
		if !c.json {
			autoRunLogf(stdout, "watch status server unavailable: %v", err)
		}
	}
	return c.runWithRuntimeFileReload(ctx, cfg, stdout, status, func(runCtx context.Context, activeCfg *config.Config) error {
		cli, err := hubClient(activeCfg)
		if err != nil {
			return err
		}
		if status != nil {
			hub, hubErr := activeCfg.DefaultHubConfig()
			if hubErr == nil {
				status.SetSummary(map[string]any{
					"hub_id":   hub.ID,
					"hub_name": hub.Name,
					"hub_host": hub.Host,
				})
			}
		}
	instance, err := buildRuntimeInstanceInfo("watch", activeCfg, status)
	if err != nil {
		return err
	}
	coord, err := newClusterRegistry(runCtx, instance)
	if err != nil {
		return err
	}
		defer coord.Close()
		return cli.Watch(runCtx, func(raw json.RawMessage) error {
			if status != nil {
				status.Add("watch event: " + string(raw))
			}
			if c.json {
				return output.JSON(stdout, json.RawMessage(raw))
			}
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				_, err := fmt.Fprintf(stdout, "%s\n", string(raw))
				return err
			}
			return output.JSON(stdout, value)
		})
	})
}

func (c command) speaker(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	c.debugf("speaker: args=%q host=%q use_hub=%t", args, c.host, c.useHub)
	if len(args) > 0 && args[0] == "test" {
		query := ""
		if len(args) > 1 {
			query = strings.Join(args[1:], " ")
		}
		c.debugf("speaker: legacy test command mapped to sound=%s query=%q", defaultSpeakerSound, query)
		return c.speakerSound(ctx, cfg, defaultSpeakerSound, query, stdout)
	}
	if len(args) > 0 && args[0] == "play" {
		if len(args) < 2 {
			return errors.New(`usage: hubhq speaker play <filename-or-url> ["speaker name"|"speaker-a,speaker-b"|speaker-all]`)
		}
		query := ""
		if len(args) > 2 {
			query = strings.Join(args[2:], " ")
		}
		c.debugf("speaker: play source=%q query=%q", args[1], query)
		return c.speakerPlay(ctx, cfg, args[1], query, stdout)
	}
	if len(args) == 0 || args[0] == "list" {
		speakers, err := listAllSpeakers(ctx, cfg)
		if err != nil {
			return err
		}
		c.persistSpeakerCache(cfg, speakers)
		c.debugf("speaker: discovered %d speaker target(s)", len(speakers))
		c.debugSpeakerTargets("speaker list", speakers)
		if c.json {
			return output.JSON(stdout, speakers)
		}
		rows := make([][]string, 0, len(speakers))
		for _, speaker := range speakers {
			rows = append(rows, []string{
				speaker.Name,
				speaker.Room,
				speaker.Playback,
				speaker.Volume,
				speaker.ID,
			})
		}
		return output.Table(stdout, []string{"NAME", "ROOM", "PLAYBACK", "VOLUME", "ID"}, rows)
	}
	return fmt.Errorf("unknown speaker command %q", args[0])
}

func (c command) speakerSound(ctx context.Context, cfg *config.Config, sound, query string, stdout io.Writer) error {
	requestedSound := sound
	sound = normalizeSpeakerSound(sound)
	if sound == "" {
		sound = defaultSpeakerSound
	}
	c.debugf("speaker sound: requested=%q normalized=%q query=%q host=%q use_hub=%t", requestedSound, sound, query, c.host, c.useHub)
	if strings.TrimSpace(query) == "" && !c.useHub {
		if c.host != "" {
			query = c.host
		} else {
			c.debugf("speaker sound: no target query provided, using local computer speaker")
			return playLocalSpeakerSound(ctx, stdout, sound)
		}
	}
	if c.host != "" {
		target := directAirPlayTarget(c.host)
		c.debugSpeakerTargets("speaker sound direct host target", []speakerTarget{target})
		return c.runSpeakerSoundTargets(ctx, cfg, sound, []speakerTarget{target}, stdout)
	}
	if strings.TrimSpace(query) != "" {
		cached := cachedSpeakerTargets(cfg.Cache)
		if len(cached) > 0 {
			c.debugf("speaker sound: trying cached speaker targets first for query=%q", query)
			c.debugSpeakerTargets("speaker sound cached targets", cached)
			targets, err := resolveSpeakerTargets(cached, query)
			if err == nil {
				if speakerTargetsNeedRefresh(targets) {
					c.debugf("speaker sound: cached target selection for query=%q needs live refresh before playback", query)
				} else {
					c.debugf("speaker sound: cache hit for query=%q targets=%d", query, len(targets))
					if err := c.runSpeakerSoundTargets(ctx, cfg, sound, targets, stdout); err == nil {
						return nil
					} else {
						c.debugf("speaker sound: cached playback failed for query=%q err=%v; retrying with live discovery", query, err)
					}
				}
			} else {
				c.debugf("speaker sound: cache miss for query=%q err=%v", query, err)
			}
		} else {
			c.debugf("speaker sound: no persisted speaker cache available for query=%q", query)
		}
	}

	speakers, err := listAllSpeakers(ctx, cfg)
	if err != nil {
		return err
	}
	c.persistSpeakerCache(cfg, speakers)
	c.debugf("speaker sound: discovered %d speaker target(s)", len(speakers))
	c.debugSpeakerTargets("speaker sound discovered targets", speakers)
	if len(speakers) == 0 {
		return errors.New("no speakers found")
	}

	targets := speakers
	if strings.TrimSpace(query) != "" {
		c.debugf("speaker sound: resolving target query=%q", query)
		targets, err = resolveSpeakerTargets(speakers, query)
		if err != nil {
			c.debugf("speaker sound: initial resolve failed for query=%q err=%v; retrying discovery", query, err)
			retrySpeakers, retryErr := listAllSpeakersWithTimeout(ctx, cfg, 8*time.Second)
			if retryErr != nil {
				return err
			}
			c.persistSpeakerCache(cfg, retrySpeakers)
			c.debugf("speaker sound: retry discovery returned %d speaker target(s)", len(retrySpeakers))
			c.debugSpeakerTargets("speaker sound retry targets", retrySpeakers)
			targets, retryErr = resolveSpeakerTargets(retrySpeakers, query)
			if retryErr != nil {
				return err
			}
		}
	}
	c.debugSpeakerTargets("speaker sound selected targets", targets)

	return c.runSpeakerSoundTargets(ctx, cfg, sound, targets, stdout)
}

func (c command) runSpeakerSoundTargets(ctx context.Context, cfg *config.Config, sound string, targets []speakerTarget, stdout io.Writer) error {
	sound = normalizeSpeakerSound(sound)
	if len(targets) == 0 {
		return playLocalSpeakerSound(ctx, stdout, sound)
	}

	names := make([]string, 0, len(targets))
	hubTargets := make([]speakerTarget, 0, len(targets))
	soundFile := ""
	playCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	profile := speakerSoundProfile(sound)
	c.debugf("speaker sound: executing sound=%s duration_ms=%d targets=%d timeout=%s", sound, profile.DurationMS, len(targets), 15*time.Second)
	for _, target := range targets {
		names = append(names, target.Name)
		c.debugf("speaker sound: target name=%s kind=%s id=%s playback=%s volume=%s sound=%s", target.Name, target.Kind, target.ID, target.Playback, target.Volume, sound)
		switch target.Kind {
		case "local":
			c.debugf("speaker sound: starting local playback for %s", target.Name)
			if err := playLocalSpeakerSound(playCtx, io.Discard, sound); err != nil {
				return err
			}
			c.debugf("speaker sound: local playback finished for %s", target.Name)
		case "cast":
			if soundFile == "" {
				var err error
				soundFile, err = generatedSpeakerSoundFile(sound)
				if err != nil {
					return err
				}
			}
			c.debugf("speaker sound: cast target host=%s port=%d file=%s", target.CastDevice.Host, target.CastDevice.Port, soundFile)
			if err := castbackend.PlayFile(playCtx, target.CastDevice, soundFile, c.debug); err != nil {
				return err
			}
			c.debugf("speaker sound: cast playback finished for %s", target.Name)
		case "airplay":
			if soundFile == "" {
				var err error
				soundFile, err = generatedSpeakerSoundFile(sound)
				if err != nil {
					return err
				}
			}
			c.debugf("speaker sound: airplay target host=%s file=%s", target.AirPlay.Host, soundFile)
			if err := airplay.PlayFile(playCtx, target.AirPlay, soundFile, cfg.Apple); err != nil {
				return err
			}
			c.debugf("speaker sound: airplay playback finished for %s", target.Name)
		case "hub":
			clip, level := hubSpeakerSoundProfile(sound)
			c.debugf("speaker sound: queueing hub playback target=%s clip=%s level=%d", target.Name, clip, level)
			hubTargets = append(hubTargets, target)
		default:
			return fmt.Errorf("unsupported speaker backend %q", target.Kind)
		}
	}
	if len(hubTargets) > 0 {
		if err := c.playHubSpeakerSound(playCtx, cfg, sound, hubTargets); err != nil {
			return err
		}
		c.debugf("speaker sound: hub playback finished for %s", strings.Join(names, ", "))
	}
	if c.json {
		return output.JSON(stdout, map[string]any{
			"speakers": names,
			"sound":    sound,
			"status":   "played",
		})
	}
	_, err := fmt.Fprintf(stdout, "played %s on %s\n", sound, strings.Join(names, ", "))
	return err
}

func (c command) speakerPlay(ctx context.Context, cfg *config.Config, source, query string, stdout io.Writer) error {
	normalizedSound := normalizeSpeakerSound(source)
	c.debugf("speaker play: source=%q normalized_sound=%q query=%q host=%q", source, normalizedSound, query, c.host)
	if normalizedSound != "" {
		c.debugf("speaker play: treating source=%q as built-in sound=%q", source, normalizedSound)
		return c.speakerSound(ctx, cfg, normalizedSound, query, stdout)
	}
	targetName := strings.TrimSpace(query)
	if c.host != "" {
		target := directAirPlayTarget(c.host)
		c.debugSpeakerTargets("speaker play direct host target", []speakerTarget{target})
		return c.runSpeakerPlayTargets(ctx, cfg, source, []speakerTarget{target}, stdout)
	}
	if targetName == "" {
		targetName = "Local Computer Speaker"
		c.debugf("speaker play: no target query provided, defaulting to %q", targetName)
	}

	speakers, err := listAllSpeakers(ctx, cfg)
	if err != nil {
		return err
	}
	c.persistSpeakerCache(cfg, speakers)
	c.debugf("speaker play: discovered %d speaker target(s)", len(speakers))
	c.debugSpeakerTargets("speaker play discovered targets", speakers)
	targets, err := resolveSpeakerTargets(speakers, targetName)
	if err != nil {
		c.debugf("speaker play: initial resolve failed for target=%q err=%v; retrying discovery", targetName, err)
		retrySpeakers, retryErr := listAllSpeakersWithTimeout(ctx, cfg, 8*time.Second)
		if retryErr != nil {
			return err
		}
		c.persistSpeakerCache(cfg, retrySpeakers)
		c.debugf("speaker play: retry discovery returned %d speaker target(s)", len(retrySpeakers))
		c.debugSpeakerTargets("speaker play retry targets", retrySpeakers)
		targets, retryErr = resolveSpeakerTargets(retrySpeakers, targetName)
		if retryErr != nil {
			return err
		}
	}
	c.debugSpeakerTargets("speaker play selected targets", targets)

	return c.runSpeakerPlayTargets(ctx, cfg, source, targets, stdout)
}

func (c command) runSpeakerPlayTargets(ctx context.Context, cfg *config.Config, source string, targets []speakerTarget, stdout io.Writer) error {
	if len(targets) == 0 {
		return errors.New("no speakers found")
	}
	resolvedSource, err := resolveSpeakerMediaSource(cfg, source)
	if err != nil {
		return err
	}
	sourceKind := "file"
	if looksLikeURL(resolvedSource) {
		sourceKind = "url"
	}
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.Name)
		c.debugf("speaker play: executing source=%q resolved_source=%q source_kind=%s target=%s kind=%s playback=%s", source, resolvedSource, sourceKind, target.Name, target.Kind, target.Playback)
		switch target.Kind {
		case "local":
			c.debugf("speaker play: starting local media playback for %s", target.Name)
			if err := playLocalMedia(ctx, resolvedSource); err != nil {
				return err
			}
			c.debugf("speaker play: local media playback finished for %s", target.Name)
		case "cast":
			c.debugf("speaker play: cast target host=%s port=%d source=%s", target.CastDevice.Host, target.CastDevice.Port, resolvedSource)
			if err := castbackend.PlayFile(ctx, target.CastDevice, resolvedSource, c.debug); err != nil {
				return err
			}
			c.debugf("speaker play: cast playback finished for %s", target.Name)
		case "airplay":
			if looksLikeURL(resolvedSource) {
				c.debugf("speaker play: airplay url host=%s source=%s", target.AirPlay.Host, resolvedSource)
				if err := airplay.PlayURL(ctx, target.AirPlay, resolvedSource, cfg.Apple); err != nil {
					return err
				}
			} else {
				c.debugf("speaker play: airplay file host=%s source=%s", target.AirPlay.Host, resolvedSource)
				if err := airplay.PlayFile(ctx, target.AirPlay, resolvedSource, cfg.Apple); err != nil {
					return err
				}
			}
			c.debugf("speaker play: airplay playback finished for %s", target.Name)
		case "hub":
			return fmt.Errorf("speaker %q does not support arbitrary media playback yet", target.Name)
		default:
			return fmt.Errorf("unsupported speaker backend %q", target.Kind)
		}
	}

	if c.json {
		return output.JSON(stdout, map[string]any{
			"speakers": names,
			"source":   resolvedSource,
			"status":   "played",
		})
	}
	_, err = fmt.Fprintf(stdout, "playing %s on %s\n", resolvedSource, strings.Join(names, ", "))
	return err
}

func directAirPlayTarget(host string) speakerTarget {
	host = strings.TrimSpace(host)
	return speakerTarget{
		ID:       firstNonEmpty(host, "airplay-host"),
		Name:     host,
		Playback: "airplay",
		Kind:     "airplay",
		AirPlay: airplay.Device{
			ID:      host,
			Name:    host,
			Host:    host,
			Port:    7000,
			Service: "_airplay._tcp",
		},
	}
}

func (c command) persistSpeakerCache(cfg *config.Config, speakers []speakerTarget) {
	if cfg == nil || len(speakers) == 0 {
		return
	}
	cfg.Cache.Speakers = make([]config.CachedSpeaker, 0, len(speakers))
	for _, speaker := range speakers {
		cfg.Cache.Speakers = append(cfg.Cache.Speakers, config.CachedSpeaker{
			ID:       speaker.ID,
			Name:     speaker.Name,
			Room:     speaker.Room,
			Playback: speaker.Playback,
			Volume:   speaker.Volume,
			Kind:     speaker.Kind,
		})
	}
	sort.Slice(cfg.Cache.Speakers, func(i, j int) bool {
		return strings.ToLower(cfg.Cache.Speakers[i].Name) < strings.ToLower(cfg.Cache.Speakers[j].Name)
	})
	if err := config.Save(cfg); err != nil {
		c.debugf("speaker cache: save failed err=%v", err)
		return
	}
	c.debugf("speaker cache: saved %d speaker target(s)", len(speakers))
}

func (c command) playHubSpeakerSound(ctx context.Context, cfg *config.Config, sound string, targets []speakerTarget) error {
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	clipName, volume := hubSpeakerSoundProfile(sound)
	commands := make([]map[string]any, 0, len(targets))
	for _, speaker := range targets {
		commands = append(commands, map[string]any{
			"type": "device",
			"id":   speaker.ID,
			"commands": []map[string]any{
				{
					"type":              "playAudioClipByName",
					"playAudioClipName": clipName,
					"volume":            volume,
				},
			},
		})
	}
	payload := map[string]any{
		"info": map[string]any{
			"name": "CLI Speaker " + normalizeSpeakerSound(sound),
			"icon": "scenes_speaker_generic",
		},
		"type":     "customScene",
		"triggers": []map[string]any{{"type": "app"}},
		"actions":  []map[string]any{},
		"commands": commands,
	}
	var created map[string]any
	if err := cli.Post(ctx, "/v1/scenes", payload, &created); err != nil {
		return err
	}
	sceneID, _ := created["id"].(string)
	if sceneID == "" {
		return errors.New("temporary speaker sound scene was created without an id")
	}
	defer func() {
		_ = cli.Delete(context.Background(), "/v1/scenes/"+sceneID)
	}()
	var resp map[string]any
	return cli.Post(ctx, "/v1/scenes/"+sceneID+"/trigger", map[string]any{}, &resp)
}

func (c command) ask(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(`usage: hubhq ask "turn on kitchen lights"`)
	}
	prompt := strings.Join(args, " ")
	c.debugf("ask: initializing LLM provider")
	provider, err := llm.NewProvider(cfg.LLM)
	if err != nil {
		return err
	}
	if logger, ok := provider.(llm.DebugLogger); ok {
		logger.SetDebugLogger(c.debugf)
	}
	c.debugf("ask: collecting inventory home context")
	homeCtx, speakerTargets, err := collectHomeContextWithSpeakers(ctx, cfg)
	if err != nil {
		return err
	}
	c.debugf("ask: collected %d devices and %d scenes", len(homeCtx.Devices), len(homeCtx.Scenes))
	c.debugf("ask: requesting executable script for prompt %q", prompt)
	askScript, err := provider.GenerateAskScript(ctx, prompt, homeCtx)
	if err != nil {
		return err
	}
	structuredRequested := wantsStructuredAskOutput(prompt)
	if structuredRequested && !hasStructuredAskResult(askScript) && strings.TrimSpace(askScript.Script.Source) == "" {
		repairPayload, _ := json.MarshalIndent(askScript, "", "  ")
		retryPrompt := prompt + "\n\nThe previous answer was invalid because it did not return usable structured output in the top-level response field.\nReturn only a valid grounded structured result in response.\nRules:\n- Do not answer with prose summary text instead of response.\n- For tables, return exactly this shape in response: {type:\"table\", headers:[\"COL1\",\"COL2\"], rows:[[\"value1\",\"value2\"]]}.\n- Table rows must contain actual grounded values from the supplied home context, not just headers.\n- If matching items exist, rows must not be empty.\n- For lists, return exactly this shape in response: {type:\"list\", items:[\"value1\",\"value2\"]}.\n- If matching items exist, items must not be empty.\n- For plain text, return response={type:\"text\", text:\"...\"}.\n- Keep NAME and ID as separate columns when both are relevant.\n- Use the supplied inventory context and do not invent values.\nPrevious invalid response:\n" + string(repairPayload)
		c.debugf("ask: retrying with stricter structured-output requirement")
		askScript, err = provider.GenerateAskScript(ctx, retryPrompt, homeCtx)
		if err != nil {
			return err
		}
	}
	c.debugJSON("ask script", askScript)
	if c.showScript && strings.TrimSpace(askScript.Script.Source) != "" {
		if _, err := fmt.Fprintf(stdout, "%s\n", askScript.Script.Source); err != nil {
			return err
		}
	}
	c.debugScript("ask script source", askScript.Script.Source)
	if askScript.RequiresConfirmation || askScript.Destructive {
		c.debugf("ask: plan blocked because it requires confirmation")
		return fmt.Errorf("llm script requires confirmation and will not auto-execute: %s", askScript.Summary)
	}
	scriptResult, result, err := c.executeAskScript(ctx, cfg, homeCtx, speakerTargets, askScript)
	if err != nil {
		if requiresStrictAskExecution(askScript) {
			c.debugf("ask: script execution failed and fallback is disabled for conditional/live script: %v", err)
			return err
		}
		c.debugf("ask: script execution failed, falling back to action bridge: %v", err)
		plan := askScriptToActionPlan(askScript)
		result, err = c.executePlannedActions(ctx, cfg, plan)
		if err != nil {
			return err
		}
	}
	c.debugf("ask: finished with result %q", result)
	if c.json {
		return output.JSON(stdout, map[string]any{
			"prompt": prompt,
			"script": askScript,
			"result": result,
			"output": func() any {
				if scriptResult == nil {
					return nil
				}
				return scriptResult.Output
			}(),
		})
	}
	if askScript.Mode == "answer" || len(askScript.Actions) == 0 {
		if scriptResult != nil {
			if ok, err := renderAskOutput(stdout, scriptResult.Output); err != nil {
				return err
			} else if ok {
				return nil
			}
		}
		if ok, err := renderAskOutput(stdout, askScript.Response); err != nil {
			return err
		} else if ok {
			return nil
		}
		if structuredRequested && strings.TrimSpace(askScript.Script.Source) == "" {
			return errors.New("llm did not return structured output for a structured ask request")
		}
		if scriptResult != nil {
			if text := askResponseText(scriptResult.Output); strings.TrimSpace(text) != "" {
				_, err := fmt.Fprintf(stdout, "%s\n", text)
				return err
			}
		}
		if responseText := askResponseText(askScript.Response); strings.TrimSpace(responseText) != "" {
			_, err := fmt.Fprintf(stdout, "%s\n", responseText)
			return err
		}
		_, err := fmt.Fprintf(stdout, "%s\n", askScript.Summary)
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s\n", result)
	return err
}

func requiresStrictAskExecution(doc *llm.AskScript) bool {
	if doc == nil {
		return false
	}
	source := strings.ToLower(strings.TrimSpace(doc.Script.Source))
	if source == "" {
		return false
	}
	for _, needle := range []string{
		"api.devices.getliveby",
		"api.devices.listliveby",
		"if(",
		"if (",
		"&&",
		"||",
	} {
		if strings.Contains(source, needle) {
			return true
		}
	}
	return false
}

func wantsStructuredAskOutput(prompt string) bool {
	normalized := strings.ToLower(strings.TrimSpace(prompt))
	for _, needle := range []string{"table", "list", "rows", "columns", "csv", "json"} {
		if strings.Contains(normalized, needle) {
			return true
		}
	}
	return false
}

func hasStructuredAskResult(doc *llm.AskScript) bool {
	if doc == nil {
		return false
	}
	object, ok := doc.Response.(map[string]any)
	if ok {
		switch strings.TrimSpace(fmt.Sprint(object["type"])) {
		case "table":
			headers, headersOK := object["headers"].([]any)
			rows, rowsOK := object["rows"].([]any)
			return headersOK && len(headers) > 0 && rowsOK && len(rows) > 0
		case "list":
			items, itemsOK := object["items"].([]any)
			return itemsOK && len(items) > 0
		}
	}
	if strings.TrimSpace(doc.Script.Source) == "" {
		return false
	}
	source := strings.ToLower(doc.Script.Source)
	return strings.Contains(source, "type: \"table\"") ||
		strings.Contains(source, "type:\"table\"") ||
		strings.Contains(source, "type: 'table'") ||
		strings.Contains(source, "type:'table'") ||
		strings.Contains(source, "type: \"list\"") ||
		strings.Contains(source, "type:\"list\"")
}

func askResponseText(value any) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if strings.TrimSpace(fmt.Sprint(object["type"])) != "text" {
		return ""
	}
	return fmt.Sprint(object["text"])
}

func (c command) auto(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(`usage: hubhq auto add "turn on hallway at sunset" | list | status | restart | show <id> | apply <id> | delete <id>`)
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return errors.New(`usage: hubhq auto add "turn on hallway at sunset"`)
		}
		return c.autoAdd(ctx, cfg, strings.Join(args[1:], " "), stdout)
	case "list":
		return c.autoList(stdout)
	case "run":
		return c.autoRun(ctx, cfg, stdout)
	case "status":
		return c.autoStatus(stdout)
	case "restart":
		return c.restartRuntimeInstances(stdout)
	case "test":
		if len(args) < 2 {
			return errors.New("usage: hubhq auto test <id>")
		}
		return c.autoTest(ctx, args[1], stdout)
	case "show":
		if len(args) < 2 {
			return c.autoShow("", stdout)
		}
		return c.autoShow(args[1], stdout)
	case "apply":
		if len(args) < 2 {
			return errors.New("usage: hubhq auto apply <id>")
		}
		return c.autoApply(args[1], stdout)
	case "delete":
		if len(args) < 2 {
			return errors.New("usage: hubhq auto delete <id>")
		}
		return c.autoDelete(args[1], stdout)
	default:
		return fmt.Errorf("unknown auto command %q", args[0])
	}
}

func (c command) autoAdd(ctx context.Context, cfg *config.Config, prompt string, stdout io.Writer) error {
	c.debugf("auto: initializing LLM provider")
	provider, err := llm.NewProvider(cfg.LLM)
	if err != nil {
		return err
	}
	if logger, ok := provider.(llm.DebugLogger); ok {
		logger.SetDebugLogger(c.debugf)
	}
	c.debugf("auto: collecting inventory home context")
	homeCtx, err := collectHomeContext(ctx, cfg)
	if err != nil {
		return err
	}
	c.debugf("auto: collected %d devices and %d scenes", len(homeCtx.Devices), len(homeCtx.Scenes))
	c.debugf("auto: requesting automation script for prompt %q", prompt)
	autoScript, err := provider.GenerateAutomationScript(ctx, prompt, homeCtx)
	if err != nil {
		return err
	}
	c.debugJSON("auto script", autoScript)
	if c.showScript && strings.TrimSpace(autoScript.Script.Source) != "" {
		if _, err := fmt.Fprintf(stdout, "%s\n", autoScript.Script.Source); err != nil {
			return err
		}
	}
	c.debugScript("auto script source", autoScript.Script.Source)
	id, err := autos.NewID()
	if err != nil {
		return err
	}
	spec := automationScriptToSpec(autoScript)
	spec.ID = id
	spec.Prompt = prompt
	if spec.ApplySupport == "" {
		spec.ApplySupport = "unsupported"
	}
	if strings.TrimSpace(spec.Script.Source) != "" && strings.EqualFold(spec.ApplySupport, "supported") {
		spec.Status = "applied"
	} else {
		spec.Status = "pending_review"
	}
	now := time.Now().UTC()
	spec.CreatedAt = now
	spec.LastUpdatedAt = now

	store, _, err := autos.Load()
	if err != nil {
		return err
	}
	store.Upsert(spec)
	if err := autos.Save(store); err != nil {
		return err
	}
	c.debugf("auto: saved spec %s with apply support %s", spec.ID, spec.ApplySupport)

	if c.json {
		return output.JSON(stdout, spec)
	}
	if spec.Status == "applied" {
		_, err = fmt.Fprintf(stdout, "saved and activated auto spec %s: %s\n", spec.ID, spec.Summary)
	} else {
		_, err = fmt.Fprintf(stdout, "saved auto spec %s: %s\n", spec.ID, spec.Summary)
	}
	return err
}

func (c command) devicesList(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	items, err := deviceCache(ctx, cfg)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, items)
	}
	sort.Slice(items, func(i, j int) bool {
		if !strings.EqualFold(items[i].Type, items[j].Type) {
			return strings.ToLower(items[i].Type) < strings.ToLower(items[j].Type)
		}
		if !strings.EqualFold(displayDeviceName(items[i]), displayDeviceName(items[j])) {
			return strings.ToLower(displayDeviceName(items[i])) < strings.ToLower(displayDeviceName(items[j]))
		}
		return strings.ToLower(items[i].Room) < strings.ToLower(items[j].Room)
	})
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{item.Name, item.Type, item.Room, item.ID})
	}
	return output.Table(stdout, []string{"NAME", "TYPE", "ROOM", "ID"}, rows)
}

func (c command) devicesGet(ctx context.Context, cfg *config.Config, query string, stdout io.Writer) error {
	device, err := resolveDevice(ctx, cfg, query)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, device)
	}
	return printMap(stdout, device)
}

func (c command) devicesSet(ctx context.Context, cfg *config.Config, query string, kv []string, stdout io.Writer) error {
	device, err := resolveDevice(ctx, cfg, query)
	if err != nil {
		return err
	}
	attrs := map[string]any{}
	for _, pair := range kv {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("invalid attribute %q, expected key=value", pair)
		}
		attrs[key] = parseScalar(value)
	}
	if isDukaOneDevice(device) {
		resp, err := dukaone.SetAttributes(ctx, cfg, idOf(device), attrs)
		if err != nil {
			return err
		}
		_ = c.refresh(ctx, cfg, io.Discard)
		if c.json {
			return output.JSON(stdout, resp)
		}
		fmt.Fprintf(stdout, "Updated device %s\n", nameOf(device))
		return nil
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	resp, err := c.patchDeviceAttributes(ctx, cli, idOf(device), attrs)
	if err != nil {
		return err
	}
	_ = c.refresh(ctx, cfg, io.Discard)
	if c.json {
		return output.JSON(stdout, resp)
	}
	fmt.Fprintf(stdout, "Updated device %s\n", nameOf(device))
	return nil
}

func (c command) devicesRename(ctx context.Context, cfg *config.Config, query, newName string, stdout io.Writer) error {
	device, err := resolveDevice(ctx, cfg, query)
	if err != nil {
		return err
	}
	if isDukaOneDevice(device) {
		if err := dukaone.Rename(cfg, idOf(device), newName); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		_ = c.refresh(ctx, cfg, io.Discard)
		if c.json {
			return output.JSON(stdout, map[string]any{"status": "renamed", "id": idOf(device), "name": newName})
		}
		fmt.Fprintf(stdout, "Renamed device %s to %s\n", nameOf(device), newName)
		return nil
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	resp, err := c.patchDeviceAttributes(ctx, cli, idOf(device), map[string]any{
		"customName": newName,
	})
	if err != nil {
		return err
	}
	_ = c.refresh(ctx, cfg, io.Discard)
	if c.json {
		return output.JSON(stdout, resp)
	}
	fmt.Fprintf(stdout, "Renamed device %s to %s\n", nameOf(device), newName)
	return nil
}

func (c command) devicesDelete(ctx context.Context, cfg *config.Config, query string, stdout io.Writer) error {
	device, err := resolveDevice(ctx, cfg, query)
	if err != nil {
		return err
	}
	if !c.yes {
		return fmt.Errorf("refusing to delete %s without --yes", nameOf(device))
	}
	if isDukaOneDevice(device) {
		if err := dukaone.Delete(cfg, idOf(device)); err != nil {
			return err
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
		_ = c.refresh(ctx, cfg, io.Discard)
		fmt.Fprintf(stdout, "Deleted device %s\n", nameOf(device))
		return nil
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	if err := cli.Delete(ctx, "/v1/devices/"+idOf(device)); err != nil {
		return err
	}
	_ = c.refresh(ctx, cfg, io.Discard)
	fmt.Fprintf(stdout, "Deleted device %s\n", nameOf(device))
	return nil
}

func (c command) scenesList(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	items, err := sceneCache(ctx, cfg)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, items)
	}
	rows := make([][]string, 0, len(items))
	for _, item := range items {
		rows = append(rows, []string{item.Name, item.Type, item.ID})
	}
	return output.Table(stdout, []string{"NAME", "TYPE", "ID"}, rows)
}

func (c command) scenesGet(ctx context.Context, cfg *config.Config, query string, stdout io.Writer) error {
	scene, err := resolveScene(ctx, cfg, query)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, scene)
	}
	return printMap(stdout, scene)
}

func (c command) scenesTrigger(ctx context.Context, cfg *config.Config, query string, stdout io.Writer) error {
	scene, err := resolveScene(ctx, cfg, query)
	if err != nil {
		return err
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	var resp map[string]any
	if err := cli.Post(ctx, "/v1/scenes/"+idOf(scene)+"/trigger", map[string]any{}, &resp); err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, resp)
	}
	fmt.Fprintf(stdout, "Triggered scene %s\n", nameOf(scene))
	return nil
}

func (c command) scenesRename(ctx context.Context, cfg *config.Config, query, newName string, stdout io.Writer) error {
	scene, err := resolveScene(ctx, cfg, query)
	if err != nil {
		return err
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	body := map[string]any{
		"info": map[string]any{
			"name": newName,
		},
	}
	var resp map[string]any
	if err := cli.Patch(ctx, "/v1/scenes/"+idOf(scene), body, &resp); err != nil {
		return err
	}
	_ = c.refresh(ctx, cfg, io.Discard)
	if c.json {
		return output.JSON(stdout, resp)
	}
	fmt.Fprintf(stdout, "Renamed scene %s to %s\n", nameOf(scene), newName)
	return nil
}

func (c command) scenesDelete(ctx context.Context, cfg *config.Config, query string, stdout io.Writer) error {
	scene, err := resolveScene(ctx, cfg, query)
	if err != nil {
		return err
	}
	if !c.yes {
		return fmt.Errorf("refusing to delete %s without --yes", nameOf(scene))
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return err
	}
	if err := cli.Delete(ctx, "/v1/scenes/"+idOf(scene)); err != nil {
		return err
	}
	_ = c.refresh(ctx, cfg, io.Discard)
	fmt.Fprintf(stdout, "Deleted scene %s\n", nameOf(scene))
	return nil
}

func (c command) autoList(stdout io.Writer) error {
	store, _, err := autos.Load()
	if err != nil {
		return err
	}
	specs := store.SortedSpecs()
	if c.json {
		return output.JSON(stdout, specs)
	}
	rows := make([][]string, 0, len(specs))
	for _, spec := range specs {
		rows = append(rows, []string{spec.ID, spec.Status, spec.ApplySupport, spec.Summary})
	}
	return output.Table(stdout, []string{"ID", "STATUS", "APPLY", "SUMMARY"}, rows)
}

func (c command) autoShow(id string, stdout io.Writer) error {
	store, _, err := autos.Load()
	if err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		specs := store.SortedSpecs()
		if c.json {
			rows := make([]map[string]string, 0, len(specs))
			for _, spec := range specs {
				rows = append(rows, map[string]string{
					"id":      spec.ID,
					"summary": spec.Summary,
				})
			}
			return output.JSON(stdout, rows)
		}
		rows := make([][]string, 0, len(specs))
		for _, spec := range specs {
			rows = append(rows, []string{spec.ID, spec.Summary})
		}
		return output.Table(stdout, []string{"ID", "SUMMARY"}, rows)
	}
	spec, ok := store.Find(id)
	if !ok {
		return fmt.Errorf("no auto spec %q", id)
	}
	return output.JSON(stdout, spec)
}

func (c command) autoApply(id string, stdout io.Writer) error {
	c.debugf("auto apply: loading stored spec %s", id)
	store, _, err := autos.Load()
	if err != nil {
		return err
	}
	spec, ok := store.Find(id)
	if !ok {
		return fmt.Errorf("no auto spec %q", id)
	}
	c.debugJSON("auto apply spec", spec)
	c.debugf("auto apply: loading current config and hub client")
	cliCfg, _, err := config.Load()
	if err != nil {
		return err
	}
	cli, err := hubClient(cliCfg)
	if err != nil {
		return err
	}
	if strings.TrimSpace(spec.Script.Source) != "" {
		c.debugf("auto apply: activating stored automation script")
		spec.Status = "applied"
		spec.LastUpdatedAt = time.Now().UTC()
		store.Upsert(spec)
		if err := autos.Save(store); err != nil {
			return err
		}
		if c.json {
			return output.JSON(stdout, map[string]any{"id": id, "applied": true, "mode": "script", "status": "active"})
		}
		_, err = fmt.Fprintf(stdout, "activated auto spec %s for auto run\n", id)
		return err
	}
	c.debugf("auto apply: compiling scene payload")
	scenePayload, err := compileAutoSpec(spec)
	if err != nil {
		spec.Status = "apply_blocked"
		spec.LastUpdatedAt = time.Now().UTC()
		store.Upsert(spec)
		_ = autos.Save(store)
		return err
	}
	c.debugJSON("auto apply payload", scenePayload)
	var resp map[string]any
	c.debugf("auto apply: creating scene on hub")
	if err := cli.Post(context.Background(), "/v1/scenes", scenePayload, &resp); err != nil {
		return err
	}
	c.debugJSON("auto apply response", resp)
	spec.Status = "applied"
	spec.LastUpdatedAt = time.Now().UTC()
	if sceneID, ok := resp["id"].(string); ok && sceneID != "" {
		spec.Notes = append(spec.Notes, "created scene id "+sceneID)
	}
	store.Upsert(spec)
	c.debugf("auto apply: saving updated spec state")
	if err := autos.Save(store); err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, map[string]any{"id": id, "applied": true, "response": resp})
	}
	_, err = fmt.Fprintf(stdout, "applied auto spec %s\n", id)
	return err
}

func (c command) autoDelete(id string, stdout io.Writer) error {
	store, _, err := autos.Load()
	if err != nil {
		return err
	}
	if !store.Delete(id) {
		return fmt.Errorf("no auto spec %q", id)
	}
	if err := autos.Save(store); err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, map[string]any{"id": id, "deleted": true})
	}
	_, err = fmt.Fprintf(stdout, "deleted auto spec %s\n", id)
	return err
}

func (c command) autoTest(ctx context.Context, id string, stdout io.Writer) error {
	store, _, err := autos.Load()
	if err != nil {
		return err
	}
	spec, ok := store.Find(id)
	if !ok {
		return fmt.Errorf("no auto spec %q", id)
	}
	if strings.TrimSpace(spec.Script.Source) == "" {
		return fmt.Errorf("auto spec %q does not contain a script", id)
	}
	cliCfg, _, err := config.Load()
	if err != nil {
		return err
	}
	cli, err := hubClient(cliCfg)
	if err != nil {
		return err
	}
	result, err := c.executeAutomationScript(ctx, cliCfg, cli, spec)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, map[string]any{
			"id":      spec.ID,
			"summary": spec.Summary,
			"result":  result,
			"mode":    "manual_test",
		})
	}
	_, err = fmt.Fprintf(stdout, "tested auto spec %s: %s\n", spec.ID, spec.Summary)
	return err
}

func (c command) autoRun(ctx context.Context, cfg *config.Config, stdout io.Writer) error {
	lastRuns := map[string]string{}
	inFlight := map[string]bool{}
	lastEventRun := map[string]time.Time{}
	status, statusErr := startRuntimeStatusServer(ctx, "auto-run", c.statusPort)
	if !c.json {
		runtimeLogf(stdout, status, "auto runner started")
		if status != nil {
			runtimeLogf(stdout, status, "auto status available at %s", status.URL())
		} else if statusErr != nil {
			c.debugf("auto status server: %v", statusErr)
			autoRunLogf(stdout, "auto status server unavailable: %v", statusErr)
		}
		store, _, err := autos.Load()
		if err != nil {
			runtimeLogf(stdout, status, "failed to load autos: %v", err)
		} else {
			active := make([]autos.Spec, 0)
			for _, spec := range store.SortedSpecs() {
				if autoSpecIsRunnerActive(spec) && strings.TrimSpace(spec.Script.Source) != "" {
					active = append(active, spec)
				}
			}
			if len(active) == 0 {
				runtimeLogf(stdout, status, "no enabled automations")
			} else {
				runtimeLogf(stdout, status, "enabled automations: %d", len(active))
				for _, spec := range active {
					runtimeLogf(stdout, status, "enabled spec %s [%s]: %s", spec.ID, spec.Trigger.Kind, spec.Summary)
				}
				if status != nil {
					items := make([]map[string]any, 0, len(active))
					for _, spec := range active {
						items = append(items, map[string]any{
							"id":      spec.ID,
							"summary": spec.Summary,
							"trigger": spec.Trigger.Kind,
							"status":  spec.Status,
						})
					}
					status.SetSummary(map[string]any{
						"enabled_automations": items,
					})
				}
			}
		}
	}
	instance, err := buildRuntimeInstanceInfo("auto-run", cfg, status)
	if err != nil {
		return err
	}
	coord, err := newClusterRegistry(ctx, instance)
	if err != nil {
		return err
	}
	defer coord.Close()
	if _, err := coord.Refresh(ctx, 1500*time.Millisecond); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		c.debugf("cluster refresh failed: %v", err)
	}
	return c.runWithRuntimeFileReload(ctx, cfg, stdout, status, func(runCtx context.Context, activeCfg *config.Config) error {
		cli, err := hubClient(activeCfg)
		if err != nil {
			return err
		}
		var outputMu sync.Mutex
		var triggerMu sync.Mutex
		go c.autoRunScheduler(runCtx, activeCfg, cli, stdout, lastRuns)
		return cli.Watch(runCtx, func(raw json.RawMessage) error {
			store, _, err := autos.Load()
			if err != nil {
				return err
			}
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				c.debugf("auto run: skipping undecodable event: %v", err)
				return nil
			}
			if c.debug {
				c.debugJSON("auto run event", payload)
			}
			for _, spec := range store.SortedSpecs() {
				if !autoSpecIsRunnerActive(spec) || strings.TrimSpace(spec.Script.Source) == "" {
					continue
				}
				if !coord.IsLeader("auto-run") {
					if c.debug {
						c.debugf("auto run: spec %s skipped: cluster leader is %s", spec.ID, coord.LeaderName("auto-run"))
					}
					continue
				}
				match, reason := autoEventMatches(spec, payload)
				if !match {
					if c.debug && reason != "" {
						c.debugf("auto run: spec %s skipped: %s", spec.ID, reason)
					}
					continue
				}
				triggerMu.Lock()
				if inFlight[spec.ID] {
					triggerMu.Unlock()
					c.debugf("auto run: spec %s skipped: already running", spec.ID)
					continue
				}
				if lastAt, ok := lastEventRun[spec.ID]; ok && time.Since(lastAt) < 2*time.Second {
					triggerMu.Unlock()
					c.debugf("auto run: spec %s skipped: debounce window", spec.ID)
					continue
				}
				inFlight[spec.ID] = true
				lastEventRun[spec.ID] = time.Now()
				triggerMu.Unlock()
				c.debugf("auto run: triggering spec %s", spec.ID)
				if !c.json {
					runtimeLogf(stdout, status, "event matched spec %s: %s [%s]", spec.ID, spec.Summary, formatAutoTriggerState(spec, payload))
				}
				specCopy := spec
				payloadCopy := cloneMap(payload)
				go func() {
					defer func() {
						triggerMu.Lock()
						delete(inFlight, specCopy.ID)
						triggerMu.Unlock()
					}()
					result, err := c.executeAutomationScript(runCtx, activeCfg, cli, specCopy)
					if err != nil {
						c.debugf("auto run: spec %s failed: %v", specCopy.ID, err)
						if !c.json {
							runtimeLogf(stdout, status, "spec %s failed: %v", specCopy.ID, err)
						}
						return
					}
					outputMu.Lock()
					defer outputMu.Unlock()
					if c.json {
						if err := output.JSON(stdout, map[string]any{
							"id":      specCopy.ID,
							"summary": specCopy.Summary,
							"result":  result,
							"event":   payloadCopy,
							"source":  "event",
						}); err != nil {
							c.debugf("auto run: spec %s output failed: %v", specCopy.ID, err)
						}
						return
					}
					runtimeLogf(stdout, status, "ran auto spec %s: %s", specCopy.ID, specCopy.Summary)
					if result != nil && result.Output != nil {
						runtimeLogf(stdout, status, "spec %s result: %v", specCopy.ID, result.Output)
					}
				}()
			}
			return nil
		})
	})
}

func (c command) autoStatus(stdout io.Writer) error {
	instances, err := queryClusterInstances(context.Background(), 3*time.Second)
	if err != nil {
		return err
	}
	if c.json {
		return output.JSON(stdout, map[string]any{
			"instances": instances,
		})
	}
	if len(instances) == 0 {
		_, err := fmt.Fprintln(stdout, "no running hubhq instances found on the LAN")
		return err
	}
	rows := make([][]string, 0, len(instances))
	leaderByGroup := clusterLeaderByGroup(instances)
	for _, instance := range instances {
		role := "standby"
		if leaderByGroup[clusterGroupKey(instance.Info)] == instance.Info.ID {
			role = "leader"
		}
		rows = append(rows, []string{
			instance.Info.Mode,
			role,
			instance.Info.Host,
			strconv.Itoa(instance.Info.PID),
			clusterInstanceStatus(instance),
			instance.Info.HubHost,
			instance.Info.StatusURL,
			instance.Info.StartedAt.Format(time.RFC3339),
		})
	}
	return output.Table(stdout, []string{"MODE", "ROLE", "HOST", "PID", "STATUS", "HUB", "URL", "STARTED"}, rows)
}

func (c command) restartRuntimeInstances(stdout io.Writer) error {
	instances, err := queryClusterInstances(context.Background(), 3*time.Second)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		_, err := fmt.Fprintln(stdout, "no running hubhq instances found on the LAN")
		return err
	}
	restarted := 0
	for _, instance := range instances {
		name := firstNonEmpty(instance.Info.Mode, "hubhq")
		host := firstNonEmpty(instance.Info.Host, instance.Info.HubHost, instance.Info.ID)
		if err := postInstanceRestart(context.Background(), instance.Info.StatusURL); err != nil {
			_, _ = fmt.Fprintf(stdout, "not restarted: %s host=%s pid=%d: %v\n", name, host, instance.Info.PID, err)
			continue
		}
		restarted++
		_, _ = fmt.Fprintf(stdout, "restarted: %s host=%s pid=%d\n", name, host, instance.Info.PID)
	}
	if restarted == 0 {
		_, err := fmt.Fprintln(stdout, "no running hubhq instances were restarted")
		return err
	}
	_, err = fmt.Fprintf(stdout, "restarted %d hubhq instance(s)\n", restarted)
	return err
}

func postInstanceRestart(ctx context.Context, statusURL string) error {
	statusURL = strings.TrimSpace(statusURL)
	if statusURL == "" {
		return fmt.Errorf("instance does not advertise a status URL")
	}
	restartURL := strings.TrimRight(statusURL, "/") + "/restart"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, restartURL, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("restart request returned %s", resp.Status)
	}
	return nil
}

func buildRuntimeInstanceInfo(mode string, cfg *config.Config, status *runtimeStatusServer) (runtimeInstanceInfo, error) {
	now := time.Now().UTC()
	host, _ := os.Hostname()
	info := runtimeInstanceInfo{
		ID:        fmt.Sprintf("%s-%d-%d", sanitizeHostID(firstNonEmpty(host, mode)), os.Getpid(), now.UnixNano()),
		Mode:      mode,
		PID:       os.Getpid(),
		StartedAt: now,
		StatusURL: "",
		Command:   strings.Join(os.Args, " "),
		Args:      append([]string(nil), os.Args...),
	}
	info.Host = host
	if status != nil {
		info.StatusURL = status.URL()
	}
	if hub, err := cfg.DefaultHubConfig(); err == nil {
		info.HubID = hub.ID
		info.HubName = hub.Name
		info.HubHost = hub.Host
		info.HubKey = firstNonEmpty(hub.ID, hub.Host)
	}
	return info, nil
}

func (c command) runWithRuntimeFileReload(ctx context.Context, cfg *config.Config, stdout io.Writer, status *runtimeStatusServer, run func(context.Context, *config.Config) error) error {
	for {
		runCtx, cancel := context.WithCancel(ctx)
		type runtimeReloadEvent struct {
			changed        []string
			restartProcess bool
		}
		changedCh := make(chan runtimeReloadEvent, 1)
		go c.monitorRuntimeFiles(runCtx, func(changed []string, restartProcess bool) {
			reloaded, _, err := config.Load()
			if err != nil {
				c.debugf("runtime reload: failed to reload config state: %v", err)
				if !c.json {
					runtimeLogf(stdout, status, "runtime reload failed for %s: %v", strings.Join(changed, ", "), err)
				}
			} else {
				*cfg = *reloaded
				c.debugf("runtime reload: applied new config state")
				if !c.json {
					runtimeLogf(stdout, status, "reloaded %s", strings.Join(changed, ", "))
				}
			}
			select {
			case changedCh <- runtimeReloadEvent{changed: changed, restartProcess: restartProcess}:
			default:
			}
			cancel()
		})
		err := run(runCtx, cfg)
		cancel()
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		}
		select {
		case event := <-changedCh:
			c.debugf("runtime reload: restarting after changes to %s", strings.Join(event.changed, ", "))
			if event.restartProcess {
				if !c.json {
					runtimeLogf(stdout, status, "detected app update, restarting process")
				}
				return restartCurrentProcess()
			}
			continue
		default:
		}
		return err
	}
}

func restartCurrentProcess() error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable for restart: %w", err)
	}
	return syscall.Exec(exePath, os.Args, os.Environ())
}

func (c command) monitorRuntimeFiles(ctx context.Context, onChange func([]string, bool)) {
	files, err := runtimeWatchedFiles()
	if err != nil {
		c.debugf("runtime reload: failed to resolve watched files: %v", err)
		return
	}
	type watchedEntry struct {
		label          string
		path           string
		restartProcess bool
	}
	watchedByDir := map[string]map[string]watchedEntry{}
	for _, file := range files {
		dir := filepath.Dir(file.path)
		if watchedByDir[dir] == nil {
			watchedByDir[dir] = map[string]watchedEntry{}
		}
		watchedByDir[dir][filepath.Base(file.path)] = watchedEntry{
			label:          file.label,
			path:           file.path,
			restartProcess: file.restartProcess,
		}
	}
	if len(watchedByDir) == 0 {
		c.debugf("runtime reload: no watched directories resolved")
		return
	}
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		c.debugf("runtime reload: failed to initialize inotify: %v", err)
		return
	}
	file := os.NewFile(uintptr(fd), "runtime-file-watcher")
	if file == nil {
		_ = unix.Close(fd)
		c.debugf("runtime reload: failed to create watcher file handle")
		return
	}
	defer file.Close()
	mask := uint32(unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_MOVE_SELF | unix.IN_CREATE | unix.IN_DELETE | unix.IN_DELETE_SELF | unix.IN_ATTRIB)
	watchDirs := map[int]string{}
	for dir := range watchedByDir {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			c.debugf("runtime reload: failed to create watched dir %s: %v", dir, err)
			return
		}
		wd, err := unix.InotifyAddWatch(fd, dir, mask)
		if err != nil {
			c.debugf("runtime reload: failed to add inotify watch for %s: %v", dir, err)
			return
		}
		watchDirs[wd] = dir
	}
	go func() {
		<-ctx.Done()
		_ = file.Close()
	}()
	buf := make([]byte, 4096)
	for {
		n, err := file.Read(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EBADF) {
				return
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			c.debugf("runtime reload: watcher read failed: %v", err)
			return
		}
		changedSet := map[string]struct{}{}
		restartProcess := false
		offset := 0
		for offset+unix.SizeofInotifyEvent <= n {
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			offset += unix.SizeofInotifyEvent
			nameBytes := buf[offset : offset+int(event.Len)]
			offset += int(event.Len)
			name := strings.TrimRight(string(nameBytes), "\x00")
			if name == "" {
				continue
			}
			dir, ok := watchDirs[int(event.Wd)]
			if !ok {
				continue
			}
			entry, ok := watchedByDir[dir][name]
			if !ok {
				continue
			}
			c.debugf("runtime reload: detected change in %s (%s)", entry.label, filepath.Join(dir, name))
			changedSet[entry.label] = struct{}{}
			if entry.restartProcess {
				restartProcess = true
			}
		}
		if len(changedSet) == 0 {
			continue
		}
		changed := make([]string, 0, len(changedSet))
		for label := range changedSet {
			changed = append(changed, label)
		}
		sort.Strings(changed)
		onChange(changed, restartProcess)
		return
	}
}

func runtimeWatchedFiles() ([]runtimeWatchedFile, error) {
	configPath, err := config.ConfigPath()
	if err != nil {
		return nil, err
	}
	tokensPath, err := config.TokensPath()
	if err != nil {
		return nil, err
	}
	cachePath, err := config.CachePath()
	if err != nil {
		return nil, err
	}
	llmCachePath, err := config.LLMCachePath()
	if err != nil {
		return nil, err
	}
	appDir, err := config.AppDir()
	if err != nil {
		return nil, err
	}
	exePath, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []runtimeWatchedFile{
		{label: "autos.json", path: filepath.Join(appDir, "autos.json")},
		{label: "cache.json", path: cachePath},
		{label: "config.json", path: configPath},
		{label: "hubhq", path: exePath, restartProcess: true},
		{label: "dukaone_bridge.py", path: filepath.Join(appDir, "dukaone_bridge.py")},
		{label: "llm-cache.json", path: llmCachePath},
		{label: "tokens.json", path: tokensPath},
	}, nil
}

func (c command) autoRunScheduler(ctx context.Context, cfg *config.Config, cli *client.Client, stdout io.Writer, lastRuns map[string]string) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := c.runScheduledSpecs(ctx, cfg, cli, stdout, lastRuns); err != nil {
			c.debugf("auto run: scheduler error: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c command) runScheduledSpecs(ctx context.Context, cfg *config.Config, cli *client.Client, stdout io.Writer, lastRuns map[string]string) error {
	store, _, err := autos.Load()
	if err != nil {
		return err
	}
	now := time.Now()
	for _, spec := range store.SortedSpecs() {
		if !autoSpecIsRunnerActive(spec) || strings.TrimSpace(spec.Script.Source) == "" {
			continue
		}
		due, runKey, reason := autoTimeMatches(spec, cfg, now)
		if !due {
			if c.debug && reason != "" {
				c.debugf("auto run: spec %s schedule skipped: %s", spec.ID, reason)
			}
			continue
		}
		if lastRuns[spec.ID] == runKey {
			continue
		}
		c.debugf("auto run: scheduled trigger for spec %s", spec.ID)
		if !c.json {
			autoRunLogf(stdout, "scheduled trigger for spec %s: %s", spec.ID, spec.Summary)
		}
		result, err := c.executeAutomationScript(ctx, cfg, cli, spec)
		if err != nil {
			c.debugf("auto run: scheduled spec %s failed: %v", spec.ID, err)
			if !c.json {
				autoRunLogf(stdout, "scheduled spec %s failed: %v", spec.ID, err)
			}
			continue
		}
		lastRuns[spec.ID] = runKey
		if c.json {
			if err := output.JSON(stdout, map[string]any{
				"id":      spec.ID,
				"summary": spec.Summary,
				"result":  result,
				"source":  "schedule",
				"run_key": runKey,
			}); err != nil {
				return err
			}
		} else {
			autoRunLogf(stdout, "ran auto spec %s on schedule: %s", spec.ID, spec.Summary)
			if result != nil && result.Output != nil {
				autoRunLogf(stdout, "scheduled spec %s result: %v", spec.ID, result.Output)
			}
		}
	}
	return nil
}

func autoSpecIsRunnerActive(spec autos.Spec) bool {
	if spec.Status == "applied" {
		return true
	}
	return spec.Status == "pending_review" &&
		strings.TrimSpace(spec.Script.Source) != "" &&
		strings.EqualFold(spec.ApplySupport, "supported")
}

func (c command) executeAutomationScript(ctx context.Context, cfg *config.Config, cli *client.Client, spec autos.Spec) (*script.Result, error) {
	c.debugf("auto run: preparing script execution for spec %s", spec.ID)
	speakerTargets := cachedSpeakerTargets(cfg.Cache)
	homeCtx := llm.HomeContext{}
	hub, hubErr := cfg.DefaultHubConfig()
	if len(cfg.Cache.Devices) > 0 || len(cfg.Cache.Scenes) > 0 || len(speakerTargets) > 0 {
		homeCtx = buildCachedLLMHomeContext(cfg.Cache, hub.Host, speakerTargets)
	} else {
		var err error
		homeCtx, speakerTargets, err = collectHomeContextWithSpeakers(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}
	if len(speakerTargets) == 0 {
		if freshSpeakers, err := listAllSpeakersWithTimeout(ctx, cfg, 2*time.Second); err == nil {
			speakerTargets = freshSpeakers
			if hubErr == nil {
				homeCtx = buildCachedLLMHomeContext(cfg.Cache, hub.Host, speakerTargets)
			}
		}
	}
	c.debugf("auto run: prepared inventory for spec %s (%d devices, %d scenes, %d speakers)", spec.ID, len(homeCtx.Devices), len(homeCtx.Scenes), len(speakerTargets))
	engine, err := script.NewEngine(spec.Script)
	if err != nil {
		return nil, err
	}
	timeoutSeconds := spec.Script.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	scriptCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	deviceService := runtimeDeviceService{
		cli:              cli,
		cfg:              cfg,
		inventoryDevices: homeCtx.Devices,
		inventoryScenes:  homeCtx.Scenes,
		speakerTargets:   speakerTargets,
	}
	speakerService := runtimeSpeakerService{cmd: c, cfg: cfg, async: true}
	stateStore := newRuntimeStateStore()
	httpClient := runtimeHTTPClient{}
	toolService := runtimeToolService{cfg: cfg}
	logger := runtimeLogger{cmd: c}
	accessService := runtimeAccessService{
		cmd:              c,
		cfg:              cfg,
		devices:          deviceService,
		deviceSnapshots:  homeCtx.Devices,
		sceneSnapshots:   homeCtx.Scenes,
		speakers:         speakerService,
		speakerSnapshots: speakerSnapshotsFromHomeContext(homeCtx),
		state:            stateStore,
		http:             httpClient,
		tools:            toolService,
		logger:           logger,
		now:              time.Now,
		stdout:           io.Discard,
		stderr:           c.stderr,
	}
	c.debugf("auto run: executing script for spec %s via runtime %s timeout=%ds", spec.ID, spec.Script.Runtime, timeoutSeconds)
	result, err := engine.Execute(scriptCtx, script.ExecContext{
		Devices:          deviceService,
		DevicesSnapshot:  homeCtx.Devices,
		ScenesSnapshot:   homeCtx.Scenes,
		Speakers:         speakerService,
		SpeakersSnapshot: speakerSnapshotsFromHomeContext(homeCtx),
		State:            stateStore,
		HTTP:             httpClient,
		Tools:            toolService,
		Access:           accessService,
		Logger:           logger,
		Now:              time.Now,
	}, spec.Script)
	if err != nil {
		if errors.Is(scriptCtx.Err(), context.DeadlineExceeded) {
			c.debugf("auto run: script execution timed out for spec %s after %ds", spec.ID, timeoutSeconds)
		}
		return nil, err
	}
	c.debugf("auto run: script execution returned for spec %s", spec.ID)
	if c.debug {
		for _, line := range result.Logs {
			c.debugf("auto script log: %s", line)
		}
		c.debugJSON("auto script result", result.Output)
	}
	return result, nil
}

func autoEventMatches(spec autos.Spec, payload map[string]any) (bool, string) {
	switch spec.Trigger.Kind {
	case "event":
		if len(spec.Trigger.DeviceIDs) == 0 && len(spec.Trigger.DeviceNames) == 0 && strings.TrimSpace(spec.Trigger.DeviceID) == "" && strings.TrimSpace(spec.Trigger.DeviceName) == "" {
			return false, "missing trigger.device_id/device_ids and trigger.device_name/device_names"
		}
		deviceIDs := append([]string{}, spec.Trigger.DeviceIDs...)
		deviceNames := append([]string{}, spec.Trigger.DeviceNames...)
		if strings.TrimSpace(spec.Trigger.DeviceID) != "" {
			deviceIDs = append(deviceIDs, spec.Trigger.DeviceID)
		}
		if strings.TrimSpace(spec.Trigger.DeviceName) != "" {
			deviceNames = append(deviceNames, spec.Trigger.DeviceName)
		}
		if !eventMatchesAnyDevice(payload, deviceIDs, deviceNames) {
			return false, "device ids/names did not match"
		}
		if spec.Trigger.Attribute != "" && !eventAttributeMatches(payload, spec.Trigger.Attribute, spec.Trigger.Value) {
			return false, "attribute value did not match"
		}
		if spec.Trigger.Notification != "" && !eventContainsValue(payload, spec.Trigger.Notification) {
			return false, "notification did not match"
		}
		return true, ""
	case "manual":
		return false, "manual trigger"
	case "time", "sun_event":
		return false, "time-based triggers are not implemented in auto run yet"
	default:
		return false, "unsupported trigger kind"
	}
}

func formatAutoTriggerState(spec autos.Spec, payload map[string]any) string {
	data, _ := payload["data"].(map[string]any)
	deviceID := stringValue(mapLookup(data, "id"))
	deviceType := stringValue(mapLookup(data, "deviceType"), mapLookup(data, "type"))
	attrs, _ := mapLookup(data, "attributes").(map[string]any)
	if spec.Trigger.Attribute != "" {
		value, ok := eventAttributeValue(payload, spec.Trigger.Attribute)
		if ok {
			if deviceID != "" {
				return fmt.Sprintf("device=%s type=%s %s=%s", deviceID, deviceType, spec.Trigger.Attribute, stringValue(value))
			}
			return fmt.Sprintf("%s=%s", spec.Trigger.Attribute, stringValue(value))
		}
	}
	if len(attrs) > 0 {
		keys := make([]string, 0, len(attrs))
		for key := range attrs {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", key, stringValue(attrs[key])))
		}
		if deviceID != "" {
			return fmt.Sprintf("device=%s type=%s %s", deviceID, deviceType, strings.Join(parts, ", "))
		}
		return strings.Join(parts, ", ")
	}
	if deviceID != "" {
		return fmt.Sprintf("device=%s type=%s", deviceID, deviceType)
	}
	return "event state unavailable"
}

func eventAttributeValue(payload map[string]any, attribute string) (any, bool) {
	data, _ := payload["data"].(map[string]any)
	if attrs, ok := data["attributes"].(map[string]any); ok {
		if value, exists := attrs[attribute]; exists {
			return value, true
		}
	}
	return nestedEventAttributeValue(payload, attribute)
}

func nestedEventAttributeValue(value any, attribute string) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == attribute {
				return child, true
			}
			if nested, ok := nestedEventAttributeValue(child, attribute); ok {
				return nested, true
			}
		}
	case []any:
		for _, child := range typed {
			if nested, ok := nestedEventAttributeValue(child, attribute); ok {
				return nested, true
			}
		}
	}
	return nil, false
}

func eventMatchesAnyDevice(payload map[string]any, deviceIDs, deviceNames []string) bool {
	for _, deviceID := range deviceIDs {
		if strings.TrimSpace(deviceID) != "" && eventContainsValue(payload, deviceID) {
			return true
		}
	}
	for _, deviceName := range deviceNames {
		if strings.TrimSpace(deviceName) != "" && eventContainsValue(payload, deviceName) {
			return true
		}
	}
	return false
}

func cloneMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	data, err := json.Marshal(input)
	if err != nil {
		out := make(map[string]any, len(input))
		for key, value := range input {
			out[key] = value
		}
		return out
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		fallback := make(map[string]any, len(input))
		for key, value := range input {
			fallback[key] = value
		}
		return fallback
	}
	return out
}

func autoTimeMatches(spec autos.Spec, cfg *config.Config, now time.Time) (bool, string, string) {
	switch spec.Trigger.Kind {
	case "time":
		if strings.TrimSpace(spec.Trigger.At) == "" {
			return false, "", "missing trigger.at"
		}
		location, err := triggerLocation(spec.Trigger.TimeZone, now.Location())
		if err != nil {
			return false, "", err.Error()
		}
		localNow := now.In(location)
		hour, minute, err := parseHourMinute(spec.Trigger.At)
		if err != nil {
			return false, "", err.Error()
		}
		if localNow.Hour() != hour || localNow.Minute() != minute {
			return false, "", "minute did not match"
		}
		return true, fmt.Sprintf("%s@%s", spec.ID, localNow.Format("2006-01-02T15:04")), ""
	case "sun_event":
		if strings.TrimSpace(spec.Trigger.SunEvent) == "" {
			return false, "", "missing trigger.sun_event"
		}
		if cfg == nil {
			return false, "", "missing config"
		}
		if cfg.Location.Latitude == 0 && cfg.Location.Longitude == 0 {
			return false, "", "missing location.latitude/location.longitude"
		}
		location, err := triggerLocation(firstNonEmpty(spec.Trigger.TimeZone, cfg.Location.TimeZone), now.Location())
		if err != nil {
			return false, "", err.Error()
		}
		eventTime, err := sunEventTime(now.In(location), cfg.Location.Latitude, cfg.Location.Longitude, spec.Trigger.SunEvent, location)
		if err != nil {
			return false, "", err.Error()
		}
		if offset := strings.TrimSpace(spec.Trigger.Offset); offset != "" {
			duration, err := parseOffsetDuration(offset)
			if err != nil {
				return false, "", err.Error()
			}
			eventTime = eventTime.Add(duration)
		}
		localNow := now.In(location)
		if localNow.Year() != eventTime.Year() || localNow.YearDay() != eventTime.YearDay() || localNow.Hour() != eventTime.Hour() || localNow.Minute() != eventTime.Minute() {
			return false, "", "minute did not match"
		}
		return true, fmt.Sprintf("%s@%s", spec.ID, eventTime.Format("2006-01-02T15:04")), ""
	case "manual":
		return false, "", "manual trigger"
	case "event":
		return false, "", "event trigger"
	default:
		return false, "", "unsupported trigger kind"
	}
}

func triggerLocation(name string, fallback *time.Location) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		if fallback != nil {
			return fallback, nil
		}
		return time.Local, nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load trigger time zone %q: %w", name, err)
	}
	return location, nil
}

func parseHourMinute(value string) (int, int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, 0, fmt.Errorf("parse trigger time %q: %w", value, err)
	}
	return parsed.Hour(), parsed.Minute(), nil
}

func parseOffsetDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	sign := time.Duration(1)
	switch value[0] {
	case '+':
		value = value[1:]
	case '-':
		sign = -1
		value = value[1:]
	}
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return 0, fmt.Errorf("parse trigger offset %q: %w", value, err)
	}
	duration := time.Duration(parsed.Hour())*time.Hour + time.Duration(parsed.Minute())*time.Minute
	return sign * duration, nil
}

func sunEventTime(day time.Time, latitude, longitude float64, event string, location *time.Location) (time.Time, error) {
	zenith := 90.833
	n := float64(day.YearDay())
	lngHour := longitude / 15.0

	var approx float64
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "sunrise":
		approx = n + ((6 - lngHour) / 24)
	case "sunset":
		approx = n + ((18 - lngHour) / 24)
	default:
		return time.Time{}, fmt.Errorf("unsupported sun event %q", event)
	}

	meanAnomaly := (0.9856 * approx) - 3.289
	trueLongitude := meanAnomaly + (1.916 * math.Sin(degToRad(meanAnomaly))) + (0.020 * math.Sin(2*degToRad(meanAnomaly))) + 282.634
	trueLongitude = normalizeDegrees(trueLongitude)

	rightAscension := radToDeg(math.Atan(0.91764 * math.Tan(degToRad(trueLongitude))))
	rightAscension = normalizeDegrees(rightAscension)
	lQuadrant := math.Floor(trueLongitude/90) * 90
	raQuadrant := math.Floor(rightAscension/90) * 90
	rightAscension = (rightAscension + (lQuadrant - raQuadrant)) / 15

	sinDec := 0.39782 * math.Sin(degToRad(trueLongitude))
	cosDec := math.Cos(math.Asin(sinDec))
	cosH := (math.Cos(degToRad(zenith)) - (sinDec * math.Sin(degToRad(latitude)))) / (cosDec * math.Cos(degToRad(latitude)))
	if cosH > 1 || cosH < -1 {
		return time.Time{}, fmt.Errorf("sun event %q is unavailable for latitude %.4f on %s", event, latitude, day.Format("2006-01-02"))
	}

	var hourAngle float64
	if strings.EqualFold(event, "sunrise") {
		hourAngle = 360 - radToDeg(math.Acos(cosH))
	} else {
		hourAngle = radToDeg(math.Acos(cosH))
	}
	hourAngle /= 15

	localMeanTime := hourAngle + rightAscension - (0.06571 * approx) - 6.622
	utcHour := normalizeHours(localMeanTime - lngHour)

	baseUTC := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	eventUTC := baseUTC.Add(time.Duration(math.Round(utcHour*60)) * time.Minute)
	return eventUTC.In(location), nil
}

func degToRad(value float64) float64 {
	return value * math.Pi / 180
}

func radToDeg(value float64) float64 {
	return value * 180 / math.Pi
}

func normalizeDegrees(value float64) float64 {
	for value < 0 {
		value += 360
	}
	for value >= 360 {
		value -= 360
	}
	return value
}

func normalizeHours(value float64) float64 {
	for value < 0 {
		value += 24
	}
	for value >= 24 {
		value -= 24
	}
	return value
}

func eventAttributeMatches(payload map[string]any, attribute string, expected any) bool {
	if attribute == "" {
		return true
	}
	candidates := []any{
		mapLookup(payload, "attributes", attribute),
		mapLookup(payload, "data", "attributes", attribute),
		mapLookup(payload, "event", "attributes", attribute),
		mapLookup(payload, attribute),
	}
	for _, value := range candidates {
		if value == nil {
			continue
		}
		if expected == nil {
			return true
		}
		if valuesEqual(value, expected) {
			return true
		}
	}
	if expected == nil && eventContainsAttribute(payload, attribute) {
		return true
	}
	return false
}

func eventContainsValue(value any, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return false
	}
	switch current := value.(type) {
	case map[string]any:
		for _, item := range current {
			if eventContainsValue(item, needle) {
				return true
			}
		}
	case []any:
		for _, item := range current {
			if eventContainsValue(item, needle) {
				return true
			}
		}
	case string:
		return strings.EqualFold(strings.TrimSpace(current), needle)
	}
	return false
}

func eventContainsAttribute(value any, attribute string) bool {
	attribute = strings.TrimSpace(attribute)
	if attribute == "" {
		return false
	}
	switch current := value.(type) {
	case map[string]any:
		for key, item := range current {
			if strings.EqualFold(strings.TrimSpace(key), attribute) {
				return true
			}
			if eventContainsAttribute(item, attribute) {
				return true
			}
		}
	case []any:
		for _, item := range current {
			if eventContainsAttribute(item, attribute) {
				return true
			}
		}
	}
	return false
}

func valuesEqual(left, right any) bool {
	switch l := left.(type) {
	case bool:
		r, ok := right.(bool)
		return ok && l == r
	case float64:
		switch rv := right.(type) {
		case float64:
			return l == rv
		case int:
			return l == float64(rv)
		}
	case string:
		return l == fmt.Sprint(right)
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `hubhq

Setup:
  hub auth                 Pair with a discovered hub and store credentials
  hub auth --host <host>   Pair directly with a hub IP/hostname
  hub list                 Show configured hubs from local config
  hub discover             Run live hub discovery on the current network
  hub probe <host>         Check TCP reachability to a hub on port 8443
  hub import-token <host> <token>

Config:
  config show              Show the full saved CLI configuration
  config get <key>         Read a saved CLI config value
  config set <key> <value> Save a CLI config value

Access:
  access show              Show resolved access directories
  access init              Create access/, data/, html/, sounds/, scripts/, skills/, tools/
  access run <script>      Run a JavaScript file from access/scripts

Ventilation:
  dukaone discover [broadcast-address]
                           Discover DukaOne ventilation devices on the network
  dukaone add              Discover and add all DukaOne devices on the network
  dukaone list             List configured DukaOne ventilation devices
  dukaone add <device-id> <name> [ip-address] [room]
  dukaone rename <name-or-id> <new-name>
  dukaone room <name-or-id> <room>
  dukaone remove <name-or-id> --yes

Devices:
  devices list             List devices
  devices get <name-or-id>
  devices set <name-or-id> key=value...
  devices rename <name-or-id> <new-name>
  devices delete <name-or-id> --yes

Scenes:
  scenes list
  scenes get <name-or-id>
  scenes trigger <name-or-id>
  scenes rename <name-or-id> <new-name>
  scenes delete <name-or-id> --yes

Media:
  speaker list             List speakers found on the local machine and network
  speaker play <source>    Play a file, URL, access/sounds file, or built-in sound-0..sound-5 on the local computer speaker
  speaker play <source> <name>
                           Play on the named speaker, a comma-separated speaker list, or speaker-all

Automation:
  ask "<prompt>"           Execute non-destructive natural-language actions
  auto add "<prompt>"      Create and store a reviewable automation spec
  auto list
  auto status              Show running hubhq instances across the LAN
  auto restart             Restart running hubhq instances across the LAN
  auto run
  auto test <id>
  auto show <id>
  auto apply <id>
  auto delete <id>

Utility:
  refresh                  Refresh local cache of devices and scenes
  dump                     Print the full home payload as JSON
  watch                    Stream update events
  watch restart            Restart running hubhq instances across the LAN

Flags:
  --version                Print the current hubhq version
  --json                   JSON output
  --yes                    Confirm destructive actions
  --debug                  Print step-level diagnostics for pairing, ask, and auto
  --show-script            Print generated script source before normal output
  --host <host>            Override hub discovery for hub auth or target a speaker host directly
  --status-port <port>     Set the browser status server port for watch and auto run (default: 8080)`)
}

func hubClient(cfg *config.Config) (*client.Client, error) {
	hub, err := cfg.DefaultHubConfig()
	if err != nil {
		return nil, err
	}
	return client.New(hub.Host, hub.Port, hub.Token), nil
}

type speakerTarget struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Room       string             `json:"room,omitempty"`
	Playback   string             `json:"playback,omitempty"`
	Volume     string             `json:"volume,omitempty"`
	Kind       string             `json:"-"`
	HubDevice  map[string]any     `json:"-"`
	CastDevice castbackend.Device `json:"-"`
	AirPlay    airplay.Device     `json:"-"`
}

func listHubSpeakers(ctx context.Context, cfg *config.Config) ([]map[string]any, error) {
	cli, err := hubClient(cfg)
	if err != nil {
		return nil, err
	}
	var devices []map[string]any
	if err := cli.Get(ctx, "/v1/devices", &devices); err != nil {
		return nil, err
	}
	speakers := make([]map[string]any, 0)
	for _, device := range devices {
		if isSpeakerLikeDevice(device) {
			speakers = append(speakers, device)
		}
	}
	sort.Slice(speakers, func(i, j int) bool {
		return strings.ToLower(nameOf(speakers[i])) < strings.ToLower(nameOf(speakers[j]))
	})
	return speakers, nil
}

func listAllSpeakers(ctx context.Context, cfg *config.Config) ([]speakerTarget, error) {
	return listAllSpeakersWithTimeout(ctx, cfg, 5*time.Second)
}

func listAllSpeakersWithTimeout(ctx context.Context, cfg *config.Config, castTimeout time.Duration) ([]speakerTarget, error) {
	entries := make([]speakerTarget, 0)
	hubSpeakers, err := listHubSpeakers(ctx, cfg)
	if err == nil {
		entries = append(entries, hubSpeakerTargets(hubSpeakers)...)
	}
	castDevices, castErr := castbackend.Discover(ctx, castTimeout)
	if castErr == nil {
		entries = append(entries, castSpeakerTargets(castDevices)...)
	}
	airplayDevices, airplayErr := airplay.Discover(ctx, castTimeout)
	if airplayErr == nil {
		entries = append(entries, airplaySpeakerTargets(airplayDevices)...)
	}
	if _, _, localErr := localSoundCommand(); localErr == nil {
		entries = append(entries, speakerTarget{
			ID:       "local-speaker",
			Name:     "Local Computer Speaker",
			Playback: "local",
			Kind:     "local",
		})
	}
	if len(entries) == 0 {
		if err != nil {
			return nil, err
		}
		if castErr != nil {
			return nil, castErr
		}
		if airplayErr != nil {
			return nil, airplayErr
		}
		return nil, errors.New("no speakers found")
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

func hubSpeakerTargets(hubSpeakers []map[string]any) []speakerTarget {
	entries := make([]speakerTarget, 0, len(hubSpeakers))
	for _, speaker := range hubSpeakers {
		entries = append(entries, speakerTarget{
			ID:        idOf(speaker),
			Name:      nameOf(speaker),
			Room:      nestedString(speaker, "room", "name"),
			Playback:  stringValue(mapLookup(speaker, "attributes", "playback")),
			Volume:    fmt.Sprint(mapLookup(speaker, "attributes", "volume")),
			Kind:      "hub",
			HubDevice: speaker,
		})
	}
	return entries
}

func castSpeakerTargets(devices []castbackend.Device) []speakerTarget {
	entries := make([]speakerTarget, 0, len(devices))
	for _, device := range devices {
		entries = append(entries, speakerTarget{
			ID:         firstNonEmpty(device.UUID, device.ID, device.Host),
			Name:       device.Name,
			Playback:   "cast",
			Volume:     "",
			Kind:       "cast",
			CastDevice: device,
		})
	}
	return entries
}

func airplaySpeakerTargets(devices []airplay.Device) []speakerTarget {
	entries := make([]speakerTarget, 0, len(devices))
	for _, device := range devices {
		entries = append(entries, speakerTarget{
			ID:       firstNonEmpty(device.ID, device.Host),
			Name:     device.Name,
			Playback: "airplay",
			Kind:     "airplay",
			AirPlay:  device,
		})
	}
	return entries
}

func resolveSpeakerTarget(entries []speakerTarget, query string) (speakerTarget, error) {
	targets, err := resolveSpeakerTargets(entries, query)
	if err != nil {
		return speakerTarget{}, err
	}
	if len(targets) != 1 {
		return speakerTarget{}, fmt.Errorf("expected exactly one speaker for %q", query)
	}
	return targets[0], nil
}

func resolveSpeakerTargets(entries []speakerTarget, query string) ([]speakerTarget, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		if len(entries) == 1 {
			return []speakerTarget{entries[0]}, nil
		}
		return nil, errors.New("multiple speakers found; specify a speaker name")
	}
	queries := splitSpeakerQueries(query)
	if len(queries) == 1 && normalizeSpeakerQuery(queries[0]) == "speaker all" {
		if len(entries) == 0 {
			return nil, errors.New("no speakers found")
		}
		return dedupeSpeakerTargets(entries), nil
	}
	resolved := make([]speakerTarget, 0, len(queries))
	for _, query := range queries {
		normalized := normalizeSpeakerQuery(query)
		if normalized == "speaker all" {
			resolved = append(resolved, entries...)
			continue
		}
		var exact []speakerTarget
		for _, entry := range entries {
			if normalizeSpeakerQuery(entry.Name) == normalized || normalizeSpeakerQuery(entry.ID) == normalized {
				exact = append(exact, entry)
			}
		}
		if len(exact) == 1 {
			resolved = append(resolved, exact[0])
			continue
		}
		if len(exact) > 1 {
			return nil, fmt.Errorf("ambiguous speaker match for %q", query)
		}
		var partial []speakerTarget
		for _, entry := range entries {
			if strings.Contains(normalizeSpeakerQuery(entry.Name), normalized) {
				partial = append(partial, entry)
			}
		}
		if len(partial) == 1 {
			resolved = append(resolved, partial[0])
			continue
		}
		if len(partial) > 1 {
			return nil, fmt.Errorf("ambiguous speaker match for %q", query)
		}
		return nil, fmt.Errorf("no speaker matched %q", query)
	}
	resolved = dedupeSpeakerTargets(resolved)
	if len(resolved) == 0 {
		return nil, errors.New("no speakers found")
	}
	return resolved, nil
}

func splitSpeakerQueries(query string) []string {
	parts := strings.Split(query, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			values = append(values, part)
		}
	}
	if len(values) == 0 && strings.TrimSpace(query) != "" {
		values = append(values, strings.TrimSpace(query))
	}
	return values
}

func dedupeSpeakerTargets(entries []speakerTarget) []speakerTarget {
	seen := map[string]bool{}
	deduped := make([]speakerTarget, 0, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.ID)
		if key == "" {
			key = normalizeSpeakerQuery(entry.Name) + "|" + entry.Kind
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, entry)
	}
	return deduped
}

func speakerTargetsNeedRefresh(targets []speakerTarget) bool {
	for _, target := range targets {
		if speakerTargetNeedsRefresh(target) {
			return true
		}
	}
	return false
}

func normalizeSpeakerQuery(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

func isSpeakerLikeDevice(device map[string]any) bool {
	if stringValue(device["type"]) == "speaker" || stringValue(device["deviceType"]) == "speaker" {
		return true
	}
	attrs, ok := device["attributes"].(map[string]any)
	if !ok {
		return false
	}
	if attrs["audioGroup"] != nil {
		return true
	}
	if attrs["playback"] != nil {
		return true
	}
	if attrs["playbackAudio"] != nil {
		return true
	}
	if attrs["volume"] != nil {
		return true
	}
	return false
}

func updateCache(cfg *config.Config, home *client.Home, externalDevices []map[string]any, speakers []speakerTarget) {
	cfg.Cache.UpdatedAt = time.Now().UTC()
	cfg.Cache.Devices = make([]config.CachedItem, 0, len(home.Devices)+len(externalDevices))
	cfg.Cache.Scenes = make([]config.CachedItem, 0, len(home.Scenes))
	cfg.Cache.Speakers = make([]config.CachedSpeaker, 0, len(speakers))

	if raw, err := json.Marshal(home); err == nil {
		cfg.Cache.RawHome = raw
	}

	allDevices := append([]map[string]any{}, home.Devices...)
	allDevices = append(allDevices, externalDevices...)
	for _, device := range allDevices {
		cfg.Cache.Devices = append(cfg.Cache.Devices, config.CachedItem{
			ID:         idOf(device),
			Name:       baseNameOf(device),
			CustomName: customNameOf(device),
			Type:       stringValue(device["type"], device["deviceType"]),
			Category:   stringValue(device["category"]),
			Room:       nestedString(device, "room", "name"),
			DeviceType: stringValue(device["deviceType"]),
		})
	}
	for _, scene := range home.Scenes {
		cfg.Cache.Scenes = append(cfg.Cache.Scenes, config.CachedItem{
			ID:   idOf(scene),
			Name: nameOf(scene),
			Type: stringValue(scene["type"]),
		})
	}
	for _, speaker := range speakers {
		cfg.Cache.Speakers = append(cfg.Cache.Speakers, config.CachedSpeaker{
			ID:       speaker.ID,
			Name:     speaker.Name,
			Room:     speaker.Room,
			Playback: speaker.Playback,
			Volume:   speaker.Volume,
			Kind:     speaker.Kind,
		})
	}

	sort.Slice(cfg.Cache.Devices, func(i, j int) bool {
		return strings.ToLower(displayDeviceName(cfg.Cache.Devices[i])) < strings.ToLower(displayDeviceName(cfg.Cache.Devices[j]))
	})
	sort.Slice(cfg.Cache.Scenes, func(i, j int) bool {
		return strings.ToLower(cfg.Cache.Scenes[i].Name) < strings.ToLower(cfg.Cache.Scenes[j].Name)
	})
	sort.Slice(cfg.Cache.Speakers, func(i, j int) bool {
		return strings.ToLower(cfg.Cache.Speakers[i].Name) < strings.ToLower(cfg.Cache.Speakers[j].Name)
	})
}

func collectHomeContext(ctx context.Context, cfg *config.Config) (llm.HomeContext, error) {
	homeCtx, _, err := collectHomeContextWithSpeakers(ctx, cfg)
	return homeCtx, err
}

func collectHomeContextWithSpeakers(ctx context.Context, cfg *config.Config) (llm.HomeContext, []speakerTarget, error) {
	hub, err := cfg.DefaultHubConfig()
	if err != nil {
		return llm.HomeContext{}, nil, err
	}

	speakerTargets := cachedSpeakerTargets(cfg.Cache)
	if len(speakerTargets) == 0 {
		liveSpeakers, speakerErr := listAllSpeakersWithTimeout(ctx, cfg, 2*time.Second)
		if speakerErr == nil {
			speakerTargets = liveSpeakers
		}
	}
	if hasCachedInventory(cfg.Cache) {
		startBackgroundInventoryRefresh(cfg)
		return buildCachedLLMHomeContext(cfg.Cache, hub.Host, speakerTargets), speakerTargets, nil
	}
	home, err := refreshInventoryCache(ctx, cfg)
	if err != nil {
		return llm.HomeContext{}, nil, err
	}
	return buildLLMHomeContext(home, hub.Host, speakerTargets, false), speakerTargets, nil
}

func cachedSpeakerTargets(cache config.Cache) []speakerTarget {
	targets := make([]speakerTarget, 0, len(cache.Speakers))
	for _, speaker := range cache.Speakers {
		targets = append(targets, speakerTarget{
			ID:       speaker.ID,
			Name:     speaker.Name,
			Room:     speaker.Room,
			Playback: speaker.Playback,
			Volume:   speaker.Volume,
			Kind:     speaker.Kind,
		})
	}
	return targets
}

func collectExecutionHomeContext(ctx context.Context, cfg *config.Config, speakerTargets []speakerTarget) (llm.HomeContext, error) {
	home, err := unifiedHome(ctx, cfg)
	if err != nil {
		return llm.HomeContext{}, err
	}
	hub, err := cfg.DefaultHubConfig()
	if err != nil {
		return llm.HomeContext{}, err
	}
	return buildLLMHomeContext(home, hub.Host, speakerTargets, true), nil
}

func hasCachedInventory(cache config.Cache) bool {
	return len(cache.Devices) > 0 || len(cache.Scenes) > 0
}

func buildCachedLLMHomeContext(cache config.Cache, hubHost string, speakerTargets []speakerTarget) llm.HomeContext {
	devices := make([]map[string]any, 0, len(cache.Devices)+len(speakerTargets))
	for _, device := range cache.Devices {
		name := strings.TrimSpace(device.CustomName)
		if name == "" {
			name = strings.TrimSpace(device.Name)
		}
		if name == "" {
			name = strings.TrimSpace(device.ID)
		}
		deviceType := strings.TrimSpace(device.Type)
		if deviceType == "" {
			deviceType = strings.TrimSpace(device.DeviceType)
		}
		room := strings.TrimSpace(device.Room)
		devices = append(devices, map[string]any{
			"id":          device.ID,
			"name":        name,
			"custom_name": strings.TrimSpace(device.CustomName),
			"type":        deviceType,
			"room":        room,
			"columns": map[string]any{
				"ID":   device.ID,
				"NAME": name,
				"TYPE": deviceType,
				"ROOM": room,
			},
			"lookup_terms": []string{
				device.ID,
				name,
				strings.TrimSpace(device.Name),
				strings.TrimSpace(device.CustomName),
				deviceType,
				room,
			},
			"capabilities":  map[string]any{},
			"status_fields": []string{},
		})
	}
	for _, speaker := range speakerTargets {
		devices = append(devices, map[string]any{
			"id":          speaker.ID,
			"name":        speaker.Name,
			"custom_name": "",
			"type":        "speaker",
			"room":        speaker.Room,
			"columns": map[string]any{
				"ID":   speaker.ID,
				"NAME": speaker.Name,
				"TYPE": "speaker",
				"ROOM": speaker.Room,
			},
			"lookup_terms":  []string{speaker.ID, speaker.Name, "speaker", speaker.Room},
			"capabilities":  map[string]any{"canReceive": []any{"playback", "volume"}},
			"status_fields": []string{"playback", "volume"},
			"speaker":       map[string]any{"kind": speaker.Kind},
		})
	}
	scenes := make([]map[string]any, 0, len(cache.Scenes))
	for _, scene := range cache.Scenes {
		scenes = append(scenes, map[string]any{
			"id":   scene.ID,
			"name": scene.Name,
			"type": scene.Type,
		})
	}
	return llm.HomeContext{
		HubHost: hubHost,
		Devices: devices,
		Scenes:  scenes,
	}
}

func buildLLMHomeContext(home *client.Home, hubHost string, speakerTargets []speakerTarget, includeLiveState bool) llm.HomeContext {
	devices := make([]map[string]any, 0, len(home.Devices)+len(speakerTargets))
	for _, device := range home.Devices {
		devices = append(devices, buildLLMDeviceItem(device, includeLiveState))
	}
	for _, speaker := range speakerTargets {
		item := map[string]any{
			"id":          speaker.ID,
			"name":        speaker.Name,
			"custom_name": "",
			"type":        "speaker",
			"room":        speaker.Room,
			"columns": map[string]any{
				"ID":   speaker.ID,
				"NAME": speaker.Name,
				"TYPE": "speaker",
				"ROOM": speaker.Room,
			},
			"lookup_terms":  []string{speaker.ID, speaker.Name, "speaker", speaker.Room},
			"capabilities":  map[string]any{"canReceive": []any{"playback", "volume"}},
			"status_fields": []string{"playback", "volume"},
			"speaker": map[string]any{
				"kind": speaker.Kind,
			},
		}
		if includeLiveState {
			item["attributes"] = map[string]any{
				"playback": speaker.Playback,
				"volume":   speaker.Volume,
			}
			item["is_reachable"] = true
		}
		devices = append(devices, item)
	}
	scenes := make([]map[string]any, 0, len(home.Scenes))
	for _, scene := range home.Scenes {
		scenes = append(scenes, buildLLMSceneItem(scene))
	}
	return llm.HomeContext{
		HubHost: hubHost,
		Devices: devices,
		Scenes:  scenes,
	}
}

func buildLLMDeviceItem(device map[string]any, includeLiveState bool) map[string]any {
	id := idOf(device)
	name := llmDeviceName(device)
	deviceType := stringValue(device["type"], device["deviceType"])
	room := nestedString(device, "room", "name")
	canReceive, _ := mapLookup(device, "capabilities", "canReceive").([]any)
	statusFields := make([]string, 0, len(canReceive))
	for _, field := range canReceive {
		statusFields = append(statusFields, fmt.Sprint(field))
	}
	item := map[string]any{
		"id":          id,
		"name":        name,
		"custom_name": customNameOf(device),
		"type":        deviceType,
		"room":        room,
		"columns": map[string]any{
			"ID":   id,
			"NAME": name,
			"TYPE": deviceType,
			"ROOM": room,
		},
		"lookup_terms":  []string{id, name, deviceType, room, customNameOf(device)},
		"capabilities":  device["capabilities"],
		"status_fields": statusFields,
	}
	if includeLiveState {
		item["attributes"] = extractAttributes(device)
		item["is_reachable"] = device["isReachable"]
		item["last_seen"] = device["lastSeen"]
	}
	return item
}

func buildLLMSceneItem(scene map[string]any) map[string]any {
	return map[string]any{
		"id":   idOf(scene),
		"name": nameOf(scene),
		"type": stringValue(scene["type"]),
	}
}

func llmDeviceName(device map[string]any) string {
	return firstNonEmpty(
		customNameOf(device),
		stringValue(mapLookup(device, "info", "name")),
		stringValue(device["customName"]),
		stringValue(device["name"]),
		idOf(device),
	)
}

func speakerSnapshotsFromHomeContext(homeCtx llm.HomeContext) []map[string]any {
	items := make([]map[string]any, 0)
	for _, device := range homeCtx.Devices {
		if !strings.EqualFold(stringValue(device["type"]), "speaker") {
			continue
		}
		items = append(items, map[string]any{
			"id":       stringValue(device["id"]),
			"name":     stringValue(device["name"]),
			"room":     stringValue(device["room"]),
			"playback": stringValue(mapLookup(device, "attributes", "playback")),
			"volume":   stringValue(mapLookup(device, "attributes", "volume")),
			"kind":     stringValue(mapLookup(device, "speaker", "kind")),
		})
	}
	return items
}

func askScriptToActionPlan(doc *llm.AskScript) *llm.ActionPlan {
	if doc == nil {
		return &llm.ActionPlan{}
	}
	return &llm.ActionPlan{
		Mode:                 doc.Mode,
		Summary:              doc.Summary,
		RequiresConfirmation: doc.RequiresConfirmation,
		Destructive:          doc.Destructive,
		Actions:              doc.Actions,
	}
}

func automationScriptToSpec(doc *llm.AutomationScript) autos.Spec {
	if doc == nil {
		return autos.Spec{}
	}
	return autos.Spec{
		Summary:      doc.Summary,
		Trigger:      doc.Trigger,
		Conditions:   doc.Conditions,
		Script:       doc.Script,
		Notes:        doc.Notes,
		ApplySupport: doc.ApplySupport,
		Actions:      []autos.Action{},
	}
}

func (c command) executeAskScript(ctx context.Context, cfg *config.Config, homeCtx llm.HomeContext, speakerTargets []speakerTarget, doc *llm.AskScript) (*script.Result, string, error) {
	if doc == nil {
		return nil, "", errors.New("ask script is missing")
	}
	if strings.TrimSpace(doc.Script.Source) == "" {
		return nil, doc.Summary, nil
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return nil, "", err
	}
	engine, err := script.NewEngine(doc.Script)
	if err != nil {
		return nil, "", err
	}
	c.debugf("ask: executing generated %s script via runtime %s", doc.Script.Language, doc.Script.Runtime)
	deviceService := runtimeDeviceService{
		cli:              cli,
		cfg:              cfg,
		inventoryDevices: homeCtx.Devices,
		inventoryScenes:  homeCtx.Scenes,
	}
	speakerService := runtimeSpeakerService{cmd: c, cfg: cfg, targets: speakerTargets}
	stateStore := newRuntimeStateStore()
	httpClient := runtimeHTTPClient{}
	toolService := runtimeToolService{cfg: cfg}
	logger := runtimeLogger{cmd: c}
	accessService := runtimeAccessService{
		cmd:              c,
		cfg:              cfg,
		devices:          deviceService,
		deviceSnapshots:  homeCtx.Devices,
		sceneSnapshots:   homeCtx.Scenes,
		speakers:         speakerService,
		speakerSnapshots: speakerSnapshotsFromHomeContext(homeCtx),
		state:            stateStore,
		http:             httpClient,
		tools:            toolService,
		logger:           logger,
		now:              time.Now,
	}
	result, err := engine.Execute(ctx, script.ExecContext{
		Devices:          deviceService,
		DevicesSnapshot:  homeCtx.Devices,
		ScenesSnapshot:   homeCtx.Scenes,
		Speakers:         speakerService,
		SpeakersSnapshot: speakerSnapshotsFromHomeContext(homeCtx),
		State:            stateStore,
		HTTP:             httpClient,
		Tools:            toolService,
		Access:           accessService,
		Logger:           logger,
		Now:              time.Now,
	}, doc.Script)
	if err != nil {
		return nil, "", err
	}
	if result != nil {
		for _, line := range result.Logs {
			c.debugf("ask script log: %s", line)
		}
	}
	if result != nil && result.Updated {
		_ = c.refresh(ctx, cfg, io.Discard)
	}
	if doc.Summary != "" {
		return result, doc.Summary, nil
	}
	if result != nil && result.Output != nil {
		return result, fmt.Sprint(result.Output), nil
	}
	return result, "script executed", nil
}

func renderAskOutput(stdout io.Writer, value any) (bool, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return false, nil
	}
	switch strings.TrimSpace(fmt.Sprint(object["type"])) {
	case "table":
		rawHeaders, ok := object["headers"].([]any)
		if !ok {
			return false, nil
		}
		rawRows, ok := object["rows"].([]any)
		if !ok {
			return false, nil
		}
		headers := make([]string, 0, len(rawHeaders))
		for _, header := range rawHeaders {
			headers = append(headers, fmt.Sprint(header))
		}
		rows := make([][]string, 0, len(rawRows))
		for _, rawRow := range rawRows {
			rowItems, ok := rawRow.([]any)
			if !ok {
				continue
			}
			row := make([]string, 0, len(rowItems))
			for _, item := range rowItems {
				row = append(row, fmt.Sprint(item))
			}
			rows = append(rows, row)
		}
		headers, rows = normalizeAskTable(headers, rows)
		return true, output.Table(stdout, headers, rows)
	case "list":
		rawItems, ok := object["items"].([]any)
		if !ok {
			return false, nil
		}
		for _, item := range rawItems {
			if _, err := fmt.Fprintf(stdout, "%s\n", fmt.Sprint(item)); err != nil {
				return true, err
			}
		}
		return true, nil
	default:
		return false, nil
	}
}

func normalizeAskTable(headers []string, rows [][]string) ([]string, [][]string) {
	nameIndex := -1
	idIndex := -1
	for i, header := range headers {
		switch strings.TrimSpace(strings.ToUpper(header)) {
		case "NAME":
			nameIndex = i
		case "ID":
			idIndex = i
		}
	}
	if nameIndex >= 0 {
		headers = removeColumn(headers, nameIndex)
		rows = removeColumnFromRows(rows, nameIndex)
		if idIndex > nameIndex {
			idIndex--
		}
	}
	if idIndex > 0 {
		headers = moveColumn(headers, idIndex, 0)
		rows = moveColumnInRows(rows, idIndex, 0)
	}
	return headers, rows
}

func removeColumn(values []string, index int) []string {
	if index < 0 || index >= len(values) {
		return values
	}
	out := make([]string, 0, len(values)-1)
	out = append(out, values[:index]...)
	out = append(out, values[index+1:]...)
	return out
}

func removeColumnFromRows(rows [][]string, index int) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		if index < 0 || index >= len(row) {
			out = append(out, row)
			continue
		}
		next := make([]string, 0, len(row)-1)
		next = append(next, row[:index]...)
		next = append(next, row[index+1:]...)
		out = append(out, next)
	}
	return out
}

func moveColumn(values []string, from, to int) []string {
	if from < 0 || from >= len(values) || to < 0 || to >= len(values) || from == to {
		return values
	}
	item := values[from]
	next := removeColumn(values, from)
	out := make([]string, 0, len(values))
	out = append(out, next[:to]...)
	out = append(out, item)
	out = append(out, next[to:]...)
	return out
}

func moveColumnInRows(rows [][]string, from, to int) [][]string {
	out := make([][]string, 0, len(rows))
	for _, row := range rows {
		if from < 0 || from >= len(row) || to < 0 || to >= len(row) || from == to {
			out = append(out, row)
			continue
		}
		item := row[from]
		next := removeColumn(row, from)
		updated := make([]string, 0, len(row))
		updated = append(updated, next[:to]...)
		updated = append(updated, item)
		updated = append(updated, next[to:]...)
		out = append(out, updated)
	}
	return out
}

func extractAttributes(device map[string]any) map[string]any {
	if attrs, ok := device["attributes"].(map[string]any); ok && attrs != nil {
		return attrs
	}
	return map[string]any{}
}

type runtimeDeviceService struct {
	cli              *client.Client
	cfg              *config.Config
	inventoryDevices []map[string]any
	inventoryScenes  []map[string]any
	speakerTargets   []speakerTarget
}

func (r runtimeDeviceService) ListDevices(ctx context.Context) ([]map[string]any, error) {
	if r.inventoryDevices != nil {
		return r.inventoryDevices, nil
	}
	homeCtx, err := collectHomeContext(ctx, r.cfg)
	if err != nil {
		return nil, err
	}
	return homeCtx.Devices, nil
}

func (r runtimeDeviceService) ListScenes(ctx context.Context) ([]map[string]any, error) {
	if r.inventoryScenes != nil {
		return r.inventoryScenes, nil
	}
	homeCtx, err := collectHomeContext(ctx, r.cfg)
	if err != nil {
		return nil, err
	}
	return homeCtx.Scenes, nil
}

func (r runtimeDeviceService) GetLiveDeviceByID(ctx context.Context, id string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	if speaker := r.liveSpeakerByID(id); speaker != nil {
		return speaker, nil
	}
	if dukaone.IsQualifiedID(id) {
		devices, err := dukaone.List(ctx, r.cfg)
		if err != nil {
			return nil, err
		}
		for _, device := range devices {
			if idOf(device) == id {
				return buildLLMDeviceItem(device, true), nil
			}
		}
		return nil, nil
	}
	var device map[string]any
	if err := r.cli.Get(ctx, "/v1/devices/"+id, &device); err != nil {
		var hubErr *client.HubError
		if errors.As(err, &hubErr) && hubErr.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return buildLLMDeviceItem(device, true), nil
}

func (r runtimeDeviceService) GetLiveDeviceByName(ctx context.Context, name string) (map[string]any, error) {
	if speaker := r.liveSpeakerByName(name); speaker != nil {
		return speaker, nil
	}
	device, err := r.resolveDevice(ctx, name)
	if err != nil {
		if strings.Contains(err.Error(), "no match") {
			return nil, nil
		}
		return nil, err
	}
	return r.GetLiveDeviceByID(ctx, idOf(device))
}

func (r runtimeDeviceService) ListLiveDevicesByRoom(ctx context.Context, roomName string) ([]map[string]any, error) {
	devices, err := r.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]map[string]any, 0)
	for _, device := range devices {
		if strings.EqualFold(stringValue(device["room"], mapLookup(device, "room", "name")), roomName) {
			matches = append(matches, device)
		}
	}
	return r.fetchLiveMatches(ctx, matches)
}

func (r runtimeDeviceService) ListLiveDevicesByType(ctx context.Context, deviceType string) ([]map[string]any, error) {
	devices, err := r.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	matches := make([]map[string]any, 0)
	for _, device := range devices {
		if strings.EqualFold(stringValue(device["type"], device["deviceType"]), deviceType) {
			matches = append(matches, device)
		}
	}
	return r.fetchLiveMatches(ctx, matches)
}

func (r runtimeDeviceService) ListLiveDevicesByName(ctx context.Context, name string) ([]map[string]any, error) {
	devices, err := r.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	query := strings.TrimSpace(strings.ToLower(name))
	matches := make([]map[string]any, 0)
	for _, device := range devices {
		for _, candidate := range []string{
			stringValue(device["name"]),
			stringValue(device["custom_name"]),
			stringValue(device["id"]),
		} {
			if strings.TrimSpace(strings.ToLower(candidate)) == query {
				matches = append(matches, device)
				break
			}
		}
	}
	if len(matches) == 0 {
		device, err := resolveByNameOrID(devices, name)
		if err == nil {
			matches = append(matches, device)
		}
	}
	return r.fetchLiveMatches(ctx, matches)
}

func (r runtimeDeviceService) SetAttributes(ctx context.Context, id string, attrs map[string]any) error {
	if dukaone.IsQualifiedID(id) {
		_, err := dukaone.SetAttributes(ctx, r.cfg, id, attrs)
		return err
	}
	var resp map[string]any
	body := []map[string]any{{"attributes": attrs}}
	return r.cli.Patch(ctx, "/v1/devices/"+id, body, &resp)
}

func (r runtimeDeviceService) SetAttributesByName(ctx context.Context, name string, attrs map[string]any) error {
	device, err := r.resolveDevice(ctx, name)
	if err != nil {
		return err
	}
	return r.SetAttributes(ctx, idOf(device), attrs)
}

func (r runtimeDeviceService) SetRoomAttributes(ctx context.Context, roomName string, attrs map[string]any) error {
	devices, err := r.ListDevices(ctx)
	if err != nil {
		return err
	}
	matched := 0
	for _, device := range devices {
		if !strings.EqualFold(stringValue(device["room"], mapLookup(device, "room", "name")), roomName) {
			continue
		}
		if err := r.SetAttributes(ctx, idOf(device), attrs); err != nil {
			return fmt.Errorf("set room attributes for %s: %w", nameOf(device), err)
		}
		matched++
	}
	if matched == 0 {
		return fmt.Errorf("no device matched room %q", roomName)
	}
	return nil
}

func (r runtimeDeviceService) TriggerScene(ctx context.Context, id string) error {
	var resp map[string]any
	return r.cli.Post(ctx, "/v1/scenes/"+id+"/trigger", map[string]any{}, &resp)
}

func (r runtimeDeviceService) TriggerSceneByName(ctx context.Context, name string) error {
	scene, err := r.resolveScene(ctx, name)
	if err != nil {
		return err
	}
	return r.TriggerScene(ctx, idOf(scene))
}

func (r runtimeDeviceService) DeleteDevice(ctx context.Context, id string) error {
	return r.cli.Delete(ctx, "/v1/devices/"+id)
}

func (r runtimeDeviceService) DeleteScene(ctx context.Context, id string) error {
	return r.cli.Delete(ctx, "/v1/scenes/"+id)
}

func (r runtimeDeviceService) resolveDevice(ctx context.Context, query string) (map[string]any, error) {
	devices, err := r.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	return resolveByNameOrID(devices, query)
}

func (r runtimeDeviceService) fetchLiveMatches(ctx context.Context, matches []map[string]any) ([]map[string]any, error) {
	if len(matches) == 0 {
		return []map[string]any{}, nil
	}
	ids := make(map[string]struct{}, len(matches))
	hasHub := false
	hasDukaOne := false
	hasSpeakers := false
	for _, match := range matches {
		ids[idOf(match)] = struct{}{}
		if strings.EqualFold(stringValue(match["type"], match["deviceType"]), "speaker") {
			hasSpeakers = true
		} else if isDukaOneDevice(match) {
			hasDukaOne = true
		} else {
			hasHub = true
		}
	}
	items := make([]map[string]any, 0, len(matches))
	if hasHub {
		var devices []map[string]any
		if err := r.cli.Get(ctx, "/v1/devices", &devices); err != nil {
			return nil, err
		}
		for _, device := range devices {
			if _, ok := ids[idOf(device)]; ok {
				items = append(items, buildLLMDeviceItem(device, true))
			}
		}
	}
	if hasDukaOne {
		devices, err := dukaone.List(ctx, r.cfg)
		if err != nil {
			return nil, err
		}
		for _, device := range devices {
			if _, ok := ids[idOf(device)]; ok {
				items = append(items, buildLLMDeviceItem(device, true))
			}
		}
	}
	if hasSpeakers {
		for _, target := range r.speakerTargets {
			if _, ok := ids[target.ID]; ok {
				items = append(items, buildLiveSpeakerDeviceItem(target))
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(stringValue(items[i]["name"], items[i]["custom_name"], items[i]["id"])) <
			strings.ToLower(stringValue(items[j]["name"], items[j]["custom_name"], items[j]["id"]))
	})
	return items, nil
}

func (r runtimeDeviceService) liveSpeakerByID(id string) map[string]any {
	for _, target := range r.speakerTargets {
		if target.ID == id {
			return buildLiveSpeakerDeviceItem(target)
		}
	}
	for _, device := range r.inventoryDevices {
		if idOf(device) == id && strings.EqualFold(stringValue(device["type"], device["deviceType"]), "speaker") {
			return map[string]any{
				"id":           idOf(device),
				"name":         stringValue(device["name"]),
				"custom_name":  stringValue(device["custom_name"]),
				"type":         "speaker",
				"room":         stringValue(device["room"]),
				"attributes":   map[string]any{},
				"is_reachable": true,
			}
		}
	}
	return nil
}

func (r runtimeDeviceService) liveSpeakerByName(name string) map[string]any {
	normalized := strings.TrimSpace(strings.ToLower(name))
	for _, target := range r.speakerTargets {
		if strings.TrimSpace(strings.ToLower(target.Name)) == normalized || strings.TrimSpace(strings.ToLower(target.ID)) == normalized {
			return buildLiveSpeakerDeviceItem(target)
		}
	}
	for _, device := range r.inventoryDevices {
		if !strings.EqualFold(stringValue(device["type"], device["deviceType"]), "speaker") {
			continue
		}
		for _, candidate := range []string{
			stringValue(device["name"]),
			stringValue(device["custom_name"]),
			idOf(device),
		} {
			if strings.TrimSpace(strings.ToLower(candidate)) == normalized {
				return map[string]any{
					"id":           idOf(device),
					"name":         stringValue(device["name"]),
					"custom_name":  stringValue(device["custom_name"]),
					"type":         "speaker",
					"room":         stringValue(device["room"]),
					"attributes":   map[string]any{},
					"is_reachable": true,
				}
			}
		}
	}
	return nil
}

func buildLiveSpeakerDeviceItem(target speakerTarget) map[string]any {
	return map[string]any{
		"id":          target.ID,
		"name":        target.Name,
		"custom_name": "",
		"type":        "speaker",
		"room":        target.Room,
		"attributes": map[string]any{
			"playback": target.Playback,
			"volume":   target.Volume,
		},
		"is_reachable": true,
		"speaker": map[string]any{
			"kind": target.Kind,
		},
	}
}

func (r runtimeDeviceService) resolveScene(ctx context.Context, query string) (map[string]any, error) {
	var scenes []map[string]any
	if err := r.cli.Get(ctx, "/v1/scenes", &scenes); err != nil {
		return nil, err
	}
	return resolveByNameOrID(scenes, query)
}

type runtimeLogger struct {
	cmd command
}

type runtimeHTTPClient struct{}

type runtimeToolService struct {
	cfg *config.Config
}

type runtimeAccessService struct {
	cmd              command
	cfg              *config.Config
	devices          script.DeviceService
	deviceSnapshots  []map[string]any
	sceneSnapshots   []map[string]any
	speakers         script.SpeakerService
	speakerSnapshots []map[string]any
	state            script.StateStore
	http             script.HTTPClient
	tools            script.ToolService
	logger           script.Logger
	now              func() time.Time
	stdinText        string
	stdout           io.Writer
	stderr           io.Writer
}

type runtimeSpeakerService struct {
	cmd     command
	cfg     *config.Config
	targets []speakerTarget
	async   bool
}

func (r runtimeSpeakerService) ListSpeakers(ctx context.Context) ([]map[string]any, error) {
	targets, err := r.snapshotTargets(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(targets))
	for _, speaker := range targets {
		items = append(items, map[string]any{
			"id":       speaker.ID,
			"name":     speaker.Name,
			"room":     speaker.Room,
			"playback": speaker.Playback,
			"volume":   speaker.Volume,
			"kind":     speaker.Kind,
		})
	}
	return items, nil
}

func (r runtimeSpeakerService) PlayTestByID(ctx context.Context, id string) error {
	return r.PlaySoundByID(ctx, id, defaultSpeakerSound)
}

func (r runtimeSpeakerService) PlaySoundByID(ctx context.Context, id, sound string) error {
	sound = normalizeSpeakerSound(sound)
	if sound == "" {
		sound = defaultSpeakerSound
	}
	targets, err := r.snapshotTargets(ctx)
	if err != nil {
		return err
	}
	for _, target := range targets {
		if target.ID == strings.TrimSpace(id) {
			if speakerTargetNeedsRefresh(target) {
				r.cmd.debugf("speaker runtime: cached target %s needs refresh before playback", target.Name)
				break
			}
			r.cmd.debugf("speaker runtime: using cached target by id %s", target.Name)
			return r.playTargets(ctx, sound, []speakerTarget{target})
		}
	}
	fresh, freshErr := listAllSpeakersWithTimeout(ctx, r.cfg, 2*time.Second)
	if freshErr == nil {
		for _, target := range fresh {
			if target.ID == strings.TrimSpace(id) {
				r.cmd.debugf("speaker runtime: using refreshed target by id %s", target.Name)
				return r.playTargets(ctx, sound, []speakerTarget{target})
			}
		}
	}
	return fmt.Errorf("no speaker matched %q", id)
}

func (r runtimeSpeakerService) PlayTestByName(ctx context.Context, name string) error {
	return r.PlaySoundByName(ctx, name, defaultSpeakerSound)
}

func (r runtimeSpeakerService) PlaySoundByName(ctx context.Context, name, sound string) error {
	sound = normalizeSpeakerSound(sound)
	if sound == "" {
		sound = defaultSpeakerSound
	}
	targets, err := r.snapshotTargets(ctx)
	if err != nil {
		return err
	}
	selected, err := resolveSpeakerTargets(targets, name)
	if err == nil && !speakerTargetsNeedRefresh(selected) {
		r.cmd.debugf("speaker runtime: using cached targets by name query=%q count=%d", name, len(selected))
		return r.playTargets(ctx, sound, selected)
	}
	if err == nil && speakerTargetsNeedRefresh(selected) {
		r.cmd.debugf("speaker runtime: cached target selection for query=%q needs refresh before playback", name)
	}
	fresh, freshErr := listAllSpeakersWithTimeout(ctx, r.cfg, 2*time.Second)
	if freshErr != nil {
		return err
	}
	selected, freshResolveErr := resolveSpeakerTargets(fresh, name)
	if freshResolveErr != nil {
		return err
	}
	r.cmd.debugf("speaker runtime: using refreshed targets by name query=%q count=%d", name, len(selected))
	return r.playTargets(ctx, sound, selected)
}

func (r runtimeSpeakerService) snapshotTargets(ctx context.Context) ([]speakerTarget, error) {
	if len(r.targets) > 0 {
		return r.targets, nil
	}
	return listAllSpeakersWithTimeout(ctx, r.cfg, 2*time.Second)
}

func speakerTargetNeedsRefresh(target speakerTarget) bool {
	switch target.Kind {
	case "cast":
		return strings.TrimSpace(target.CastDevice.Host) == "" || target.CastDevice.Port <= 0
	case "airplay":
		return strings.TrimSpace(target.AirPlay.Host) == ""
	default:
		return false
	}
}

func (r runtimeSpeakerService) playTargets(ctx context.Context, sound string, targets []speakerTarget) error {
	if !r.async {
		return r.cmd.runSpeakerSoundTargets(ctx, r.cfg, sound, targets, io.Discard)
	}
	playCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	go func() {
		defer cancel()
		if err := r.cmd.runSpeakerSoundTargets(playCtx, r.cfg, sound, targets, io.Discard); err != nil {
			r.cmd.debugf("speaker runtime: async playback failed: %v", err)
			return
		}
		r.cmd.debugf("speaker runtime: async playback finished")
	}()
	return nil
}

func (r runtimeLogger) Printf(format string, args ...any) {
	r.cmd.debugf("script: "+format, args...)
}

func (runtimeHTTPClient) Do(ctx context.Context, method, url string, body any, headers map[string]string) (map[string]any, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead:
	default:
		return nil, fmt.Errorf("unsupported http method %q", method)
	}
	parsed, err := neturl.Parse(strings.TrimSpace(url))
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported url scheme %q", parsed.Scheme)
	}

	var reader io.Reader
	switch value := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(value)
	case []byte:
		reader = bytes.NewReader(value)
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode http body: %w", err)
		}
		reader = bytes.NewReader(data)
		if headers == nil {
			headers = map[string]string{}
		}
		if strings.TrimSpace(headers["Content-Type"]) == "" {
			headers["Content-Type"] = "application/json"
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, parsed.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for key, value := range headers {
		if strings.TrimSpace(key) == "" {
			continue
		}
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("perform request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	headerMap := map[string]any{}
	for key, values := range resp.Header {
		if len(values) == 1 {
			headerMap[key] = values[0]
			continue
		}
		items := make([]any, 0, len(values))
		for _, value := range values {
			items = append(items, value)
		}
		headerMap[key] = items
	}
	result := map[string]any{
		"url":         parsed.String(),
		"method":      method,
		"ok":          resp.StatusCode >= 200 && resp.StatusCode < 300,
		"status":      resp.StatusCode,
		"status_text": resp.Status,
		"headers":     headerMap,
		"body_text":   string(raw),
	}
	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if strings.Contains(contentType, "application/json") {
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err == nil {
			result["json"] = decoded
		}
	}
	return result, nil
}

func (r runtimeToolService) Run(ctx context.Context, name string, args []string, stdin string, timeoutSeconds int) (map[string]any, error) {
	path, err := r.resolveTool(name)
	if err != nil {
		return nil, err
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, path, args...)
	cmd.Dir = filepath.Dir(path)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	err = cmd.Run()
	durationMS := time.Since(started).Milliseconds()
	exitCode := 0
	ok := true
	if err != nil {
		ok = false
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			exitCode = -1
		} else {
			return nil, fmt.Errorf("run tool %s: %w", path, err)
		}
	}
	return map[string]any{
		"ok":          ok,
		"exit_code":   exitCode,
		"stdout":      stdout.String(),
		"stderr":      stderr.String(),
		"command":     path,
		"args":        args,
		"duration_ms": durationMS,
	}, nil
}

func (r runtimeToolService) resolveTool(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("tool name is required")
	}
	if err := config.EnsureAccessLayout(r.cfg); err != nil {
		return "", err
	}
	base, err := config.AccessToolsDir(r.cfg)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute tool paths are not allowed: %s", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("tool path escapes tools scope: %s", name)
	}
	resolved := filepath.Join(base, clean)
	baseClean := filepath.Clean(base)
	resolvedClean := filepath.Clean(resolved)
	if resolvedClean != baseClean && !strings.HasPrefix(resolvedClean, baseClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("tool path escapes tools scope: %s", name)
	}
	info, err := os.Stat(resolvedClean)
	if err != nil {
		return "", fmt.Errorf("stat tool path %s: %w", resolvedClean, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("tool path is a directory: %s", name)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("tool is not executable: %s", name)
	}
	return resolvedClean, nil
}

func (r runtimeAccessService) Root(ctx context.Context) (string, error) {
	_ = ctx
	if err := config.EnsureAccessLayout(r.cfg); err != nil {
		return "", err
	}
	return config.AccessRoot(r.cfg)
}

func (r runtimeAccessService) List(ctx context.Context, scope, path string) ([]map[string]any, error) {
	_ = ctx
	dir, err := r.resolve(scope, path, true)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list access path %s: %w", dir, err)
	}
	items := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat access path %s: %w", entry.Name(), err)
		}
		items = append(items, map[string]any{
			"name":     entry.Name(),
			"is_dir":   entry.IsDir(),
			"size":     info.Size(),
			"mod_time": info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(stringValue(items[i]["name"])) < strings.ToLower(stringValue(items[j]["name"]))
	})
	return items, nil
}

func (r runtimeAccessService) ReadText(ctx context.Context, scope, path string) (string, error) {
	_ = ctx
	resolved, err := r.resolve(scope, path, false)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read access file %s: %w", resolved, err)
	}
	return string(data), nil
}

func (r runtimeAccessService) WriteText(ctx context.Context, scope, path, value string) error {
	_ = ctx
	resolved, err := r.resolve(scope, path, false)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return fmt.Errorf("create access parent dir: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write access file %s: %w", resolved, err)
	}
	return nil
}

func (r runtimeAccessService) ReadJSON(ctx context.Context, scope, path string) (any, error) {
	value, err := r.ReadText(ctx, scope, path)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, fmt.Errorf("decode access json %s/%s: %w", scope, path, err)
	}
	return decoded, nil
}

func (r runtimeAccessService) WriteJSON(ctx context.Context, scope, path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal access json %s/%s: %w", scope, path, err)
	}
	data = append(data, '\n')
	return r.WriteText(ctx, scope, path, string(data))
}

func (r runtimeAccessService) Delete(ctx context.Context, scope, path string) error {
	_ = ctx
	resolved, err := r.resolve(scope, path, false)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(resolved); err != nil {
		return fmt.Errorf("delete access path %s: %w", resolved, err)
	}
	return nil
}

func (r runtimeAccessService) ListScripts(ctx context.Context) ([]map[string]any, error) {
	return r.List(ctx, "scripts", ".")
}

func (r runtimeAccessService) RunScript(ctx context.Context, name string, args []string, input any) (any, error) {
	path, err := r.resolveScriptName(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read access script %s: %w", path, err)
	}
	doc := script.Document{
		Language:   script.LanguageJavaScript,
		Runtime:    script.RuntimeGoja,
		EntryPoint: "main",
		Source:     string(data),
	}
	engine, err := script.NewEngine(doc)
	if err != nil {
		return nil, err
	}
	execCtx := script.ExecContext{
		Devices:          r.devices,
		DevicesSnapshot:  r.deviceSnapshots,
		ScenesSnapshot:   r.sceneSnapshots,
		Speakers:         r.speakers,
		SpeakersSnapshot: r.speakerSnapshots,
		State:            r.state,
		HTTP:             r.http,
		Tools:            r.tools,
		Logger:           r.logger,
		Now:              r.now,
		ScriptArgs:       args,
		ScriptInput:      input,
		StdinText:        r.stdinText,
		Stdout:           r.stdout,
		Stderr:           r.stderr,
	}
	access := runtimeAccessService{
		cmd:              r.cmd,
		cfg:              r.cfg,
		devices:          r.devices,
		deviceSnapshots:  r.deviceSnapshots,
		sceneSnapshots:   r.sceneSnapshots,
		speakers:         r.speakers,
		speakerSnapshots: r.speakerSnapshots,
		state:            r.state,
		http:             r.http,
		tools:            r.tools,
		logger:           r.logger,
		now:              r.now,
		stdinText:        r.stdinText,
		stdout:           r.stdout,
		stderr:           r.stderr,
	}
	execCtx.Access = access
	result, err := engine.Execute(ctx, execCtx, doc)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	if r.logger != nil {
		for _, line := range result.Logs {
			r.logger.Printf("%s", line)
		}
	}
	return result.Output, nil
}

func (r runtimeAccessService) resolve(scope, rel string, allowDir bool) (string, error) {
	if err := config.EnsureAccessLayout(r.cfg); err != nil {
		return "", err
	}
	var base string
	var err error
	switch strings.TrimSpace(strings.ToLower(scope)) {
	case "data":
		base, err = config.AccessDataDir(r.cfg)
	case "html":
		base, err = config.AccessHTMLDir(r.cfg)
	case "sounds":
		base, err = config.AccessSoundsDir(r.cfg)
	case "scripts":
		base, err = config.AccessScriptsDir(r.cfg)
	case "skills":
		base, err = config.AccessSkillsDir(r.cfg)
	case "tools":
		base, err = config.AccessToolsDir(r.cfg)
	default:
		return "", fmt.Errorf("unsupported access scope %q", scope)
	}
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		rel = "."
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute access paths are not allowed: %s", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("access path escapes scope %q: %s", scope, rel)
	}
	resolved := filepath.Join(base, clean)
	baseClean := filepath.Clean(base)
	resolvedClean := filepath.Clean(resolved)
	if resolvedClean != baseClean && !strings.HasPrefix(resolvedClean, baseClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("access path escapes scope %q: %s", scope, rel)
	}
	if allowDir {
		info, err := os.Stat(resolvedClean)
		if err != nil {
			return "", fmt.Errorf("stat access path %s: %w", resolvedClean, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("access path is not a directory: %s", rel)
		}
	}
	return resolvedClean, nil
}

func (r runtimeAccessService) resolveScriptName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("script name is required")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".js") {
		name += ".js"
	}
	return r.resolve("scripts", name, false)
}

type runtimeStateStore struct {
	mu   sync.Mutex
	path string
}

func newRuntimeStateStore() *runtimeStateStore {
	dir, err := config.AppDir()
	if err != nil {
		return &runtimeStateStore{path: filepath.Join(".", "script-state.json")}
	}
	return &runtimeStateStore{path: filepath.Join(dir, "script-state.json")}
}

func (s *runtimeStateStore) Snapshot() (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *runtimeStateStore) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return nil, false
	}
	value, ok := state[key]
	return value, ok
}

func (s *runtimeStateStore) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return err
	}
	state[key] = value
	return s.save(state)
}

func (s *runtimeStateStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return err
	}
	delete(state, key)
	return s.save(state)
}

func (s *runtimeStateStore) load() (map[string]any, error) {
	state := map[string]any{}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *runtimeStateStore) save(state map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(s.path, data, 0o600)
}

func (c command) executePlannedActions(ctx context.Context, cfg *config.Config, plan *llm.ActionPlan) (string, error) {
	if plan.Mode == "answer" || len(plan.Actions) == 0 {
		c.debugf("ask: no executable actions in plan")
		return plan.Summary, nil
	}
	cli, err := hubClient(cfg)
	if err != nil {
		return "", err
	}
	var executed []string
	var failed []string
	for i, action := range plan.Actions {
		target := firstNonEmpty(action.TargetName, action.TargetID)
		c.debugf("ask: executing action %d/%d type=%s target=%s", i+1, len(plan.Actions), action.Type, target)
		var actionErr error
		switch action.Type {
		case "set_device_attributes":
			if _, err := c.patchDeviceAttributes(ctx, cli, action.TargetID, action.Attributes); err != nil {
				actionErr = fmt.Errorf("ask action failed for %s (%s): %w", target, action.Type, err)
			} else {
				executed = append(executed, fmt.Sprintf("updated %s", target))
			}
		case "trigger_scene":
			path := "/v1/scenes/" + action.TargetID + "/trigger"
			body := map[string]any{}
			c.debugf("ask: post %s", path)
			c.debugJSON("ask request", body)
			var resp map[string]any
			if err := cli.Post(ctx, path, body, &resp); err != nil {
				actionErr = fmt.Errorf("ask action failed for %s (%s): %w", target, action.Type, err)
			} else {
				c.debugJSON("ask response", resp)
				executed = append(executed, fmt.Sprintf("triggered %s", target))
			}
		case "rename_device":
			if _, err := c.patchDeviceAttributes(ctx, cli, action.TargetID, map[string]any{
				"customName": action.Attributes["customName"],
			}); err != nil {
				actionErr = fmt.Errorf("ask action failed for %s (%s): %w", target, action.Type, err)
			} else {
				executed = append(executed, fmt.Sprintf("renamed %s", action.TargetID))
			}
		case "rename_scene":
			path := "/v1/scenes/" + action.TargetID
			body := map[string]any{
				"info": map[string]any{"name": action.Attributes["name"]},
			}
			c.debugf("ask: patch %s", path)
			c.debugJSON("ask request", body)
			var resp map[string]any
			if err := cli.Patch(ctx, path, body, &resp); err != nil {
				actionErr = fmt.Errorf("ask action failed for %s (%s): %w", target, action.Type, err)
			} else {
				c.debugJSON("ask response", resp)
				executed = append(executed, fmt.Sprintf("renamed %s", action.TargetID))
			}
		case "set_speaker_volume":
			if _, err := c.patchDeviceAttributes(ctx, cli, action.TargetID, map[string]any{
				"volume": action.Attributes["volume"],
			}); err != nil {
				actionErr = fmt.Errorf("ask action failed for %s (%s): %w", target, action.Type, err)
			} else {
				executed = append(executed, fmt.Sprintf("set volume on %s", target))
			}
		case "set_speaker_playback":
			playback := fmt.Sprint(action.Value)
			if playback == "" {
				playback = fmt.Sprint(action.Attributes["playback"])
			}
			if playback == "" {
				return "", fmt.Errorf("set_speaker_playback action is missing playback value")
			}
			if _, err := c.patchDeviceAttributes(ctx, cli, action.TargetID, map[string]any{
				"playback": playback,
			}); err != nil {
				actionErr = fmt.Errorf("ask action failed for %s (%s): %w", target, action.Type, err)
			} else {
				executed = append(executed, fmt.Sprintf("set playback on %s to %s", target, playback))
			}
		case "delete_device", "delete_scene":
			return "", fmt.Errorf("destructive actions from `ask` require confirmation and are blocked")
		default:
			return "", fmt.Errorf("unsupported action type %q", action.Type)
		}
		if actionErr != nil {
			c.debugf("ask: action failed target=%s err=%v", target, actionErr)
			failed = append(failed, actionErr.Error())
		}
	}
	c.debugf("ask: refreshing local cache after execution")
	_ = c.refresh(ctx, cfg, io.Discard)
	if len(failed) == len(plan.Actions) {
		return "", errors.New(strings.Join(failed, "; "))
	}
	if len(failed) > 0 {
		summary := plan.Summary
		if summary == "" {
			summary = strings.Join(executed, ", ")
		}
		return fmt.Sprintf("%s (%d succeeded, %d failed: %s)", summary, len(executed), len(failed), strings.Join(failed, "; ")), nil
	}
	if plan.Summary != "" {
		return plan.Summary, nil
	}
	return strings.Join(executed, ", "), nil
}

func compileAutoSpec(spec autos.Spec) (map[string]any, error) {
	if spec.Trigger.Kind != "event" {
		return nil, fmt.Errorf("auto apply currently supports only event-triggered specs")
	}
	if len(spec.Trigger.DeviceIDs) > 1 || len(spec.Trigger.DeviceNames) > 1 {
		return nil, fmt.Errorf("auto apply currently supports only a single event trigger device")
	}
	if spec.Trigger.DeviceID == "" {
		return nil, fmt.Errorf("event-triggered auto spec is missing trigger.device_id")
	}

	trigger := map[string]any{
		"type": "event",
		"trigger": map[string]any{
			"days":     []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
			"deviceId": spec.Trigger.DeviceID,
			"filter": map[string]any{
				"attribute": map[string]any{},
			},
		},
	}
	if spec.Trigger.Attribute != "" {
		trigger["trigger"].(map[string]any)["filter"].(map[string]any)["attribute"].(map[string]any)[spec.Trigger.Attribute] = spec.Trigger.Value
	}
	if spec.Trigger.Notification != "" {
		trigger["trigger"].(map[string]any)["notifications"] = []map[string]any{{"type": spec.Trigger.Notification}}
	}

	var actions []map[string]any
	var commands []map[string]any
	for _, action := range spec.Actions {
		switch action.Type {
		case "set_device_attributes":
			if action.TargetID == "" {
				return nil, fmt.Errorf("set_device_attributes action is missing target_id")
			}
			actions = append(actions, map[string]any{
				"type":       "device",
				"deviceId":   action.TargetID,
				"enabled":    true,
				"attributes": action.Attributes,
			})
		case "play_audio_clip":
			if action.TargetID == "" {
				return nil, fmt.Errorf("play_audio_clip action is missing target_id")
			}
			clip := action.ClipName
			if clip == "" {
				clip = defaultClipForSpec(spec)
			}
			if clip != audioClipOpened && clip != audioClipClosed {
				return nil, fmt.Errorf("unsupported clip %q; only built-in open/close clips are supported", clip)
			}
			volume := action.Volume
			if volume <= 0 {
				volume = 20
			}
			commands = append(commands, map[string]any{
				"type": "device",
				"id":   action.TargetID,
				"commands": []map[string]any{
					{
						"type":              "playAudioClipByName",
						"playAudioClipName": clip,
						"volume":            volume,
					},
				},
			})
		default:
			return nil, fmt.Errorf("auto apply does not support action type %q", action.Type)
		}
	}
	if len(actions) == 0 && len(commands) == 0 {
		return nil, fmt.Errorf("auto spec has no supported actions to apply")
	}

	payload := map[string]any{
		"info": map[string]any{
			"name": firstNonEmpty(spec.Summary, spec.Prompt, "Generated Auto"),
			"icon": "scenes_speaker_generic",
		},
		"type":     "customScene",
		"triggers": []map[string]any{trigger},
		"actions":  actions,
		"commands": commands,
	}
	return payload, nil
}

func defaultClipForSpec(spec autos.Spec) string {
	if spec.Trigger.Notification == "closed" {
		return audioClipClosed
	}
	if strings.Contains(strings.ToLower(spec.Prompt), "close") {
		return audioClipClosed
	}
	return audioClipOpened
}

func deviceCache(ctx context.Context, cfg *config.Config) ([]config.CachedItem, error) {
	if len(cfg.Cache.Devices) == 0 {
		if err := (command{}).refresh(ctx, cfg, io.Discard); err != nil {
			return nil, err
		}
	}
	return cfg.Cache.Devices, nil
}

func sceneCache(ctx context.Context, cfg *config.Config) ([]config.CachedItem, error) {
	if len(cfg.Cache.Scenes) == 0 {
		if err := (command{}).refresh(ctx, cfg, io.Discard); err != nil {
			return nil, err
		}
	}
	return cfg.Cache.Scenes, nil
}

func unifiedHome(ctx context.Context, cfg *config.Config) (*client.Home, error) {
	return fetchUnifiedHome(ctx, cfg)
}

func fetchUnifiedHome(ctx context.Context, cfg *config.Config) (*client.Home, error) {
	cli, err := hubClient(cfg)
	if err != nil {
		return nil, err
	}
	home, err := cli.Home(ctx)
	if err != nil {
		return nil, err
	}
	externalDevices, err := dukaone.List(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if len(externalDevices) > 0 {
		home.Devices = append(home.Devices, externalDevices...)
	}
	return home, nil
}

func refreshInventoryCache(ctx context.Context, cfg *config.Config) (*client.Home, error) {
	cli, err := hubClient(cfg)
	if err != nil {
		return nil, err
	}
	home, err := cli.Home(ctx)
	if err != nil {
		return nil, err
	}
	externalDevices, err := dukaone.List(ctx, cfg)
	if err != nil {
		return nil, err
	}
	speakers, speakerErr := listAllSpeakersWithTimeout(ctx, cfg, 2*time.Second)
	if speakerErr != nil {
		speakers = cachedSpeakerTargets(cfg.Cache)
	}
	updateCache(cfg, home, externalDevices, speakers)
	if err := config.Save(cfg); err != nil {
		return nil, err
	}
	if len(externalDevices) > 0 {
		home.Devices = append(home.Devices, externalDevices...)
	}
	return home, nil
}

func startBackgroundInventoryRefresh(cfg *config.Config) {
	inventoryRefreshState.mu.Lock()
	if inventoryRefreshState.running {
		inventoryRefreshState.mu.Unlock()
		return
	}
	inventoryRefreshState.running = true
	inventoryRefreshState.mu.Unlock()

	go func() {
		defer func() {
			inventoryRefreshState.mu.Lock()
			inventoryRefreshState.running = false
			inventoryRefreshState.mu.Unlock()
		}()
		refreshCfg, _, err := config.Load()
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = refreshInventoryCache(ctx, refreshCfg)
	}()
}

func resolveDevice(ctx context.Context, cfg *config.Config, query string) (map[string]any, error) {
	home, err := unifiedHome(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return resolveByNameOrID(home.Devices, query)
}

func resolveScene(ctx context.Context, cfg *config.Config, query string) (map[string]any, error) {
	cli, err := hubClient(cfg)
	if err != nil {
		return nil, err
	}
	var scenes []map[string]any
	if err := cli.Get(ctx, "/v1/scenes", &scenes); err != nil {
		return nil, err
	}
	return resolveByNameOrID(scenes, query)
}

func resolveByNameOrID(items []map[string]any, query string) (map[string]any, error) {
	var exactID []map[string]any
	var exactName []map[string]any
	lower := strings.ToLower(query)
	for _, item := range items {
		if idOf(item) == query {
			exactID = append(exactID, item)
		}
		if stringValue(mapLookup(item, "dukaone", "deviceId")) == query {
			exactID = append(exactID, item)
		}
		if strings.ToLower(nameOf(item)) == lower {
			exactName = append(exactName, item)
		}
	}
	if len(exactID) == 1 {
		return exactID[0], nil
	}
	if len(exactName) == 1 {
		return exactName[0], nil
	}
	if len(exactID) > 1 || len(exactName) > 1 {
		return nil, fmt.Errorf("ambiguous match for %q", query)
	}
	return nil, fmt.Errorf("no item matched %q", query)
}

func chooseHub(stdin io.Reader, stdout io.Writer, hubs []discovery.Hub) (discovery.Hub, error) {
	if len(hubs) == 1 {
		return hubs[0], nil
	}
	fmt.Fprintln(stdout, "Multiple hubs discovered:")
	for i, hub := range hubs {
		fmt.Fprintf(stdout, "  %d. %s (%s:%d)\n", i+1, hub.Name, hub.Host, hub.Port)
	}
	fmt.Fprint(stdout, "Select a hub number: ")
	line, err := bufio.NewReader(stdin).ReadString('\n')
	if err != nil {
		return discovery.Hub{}, err
	}
	index, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || index < 1 || index > len(hubs) {
		return discovery.Hub{}, errors.New("invalid hub selection")
	}
	return hubs[index-1], nil
}

func printMap(w io.Writer, item map[string]any) error {
	keys := output.SortedMapKeys(item)
	for _, key := range keys {
		value, _ := json.MarshalIndent(item[key], "", "  ")
		if _, err := fmt.Fprintf(w, "%s: %s\n", key, strings.TrimSpace(string(value))); err != nil {
			return err
		}
	}
	return nil
}

func idOf(item map[string]any) string {
	return stringValue(item["id"])
}

func customNameOf(item map[string]any) string {
	if value := stringValue(item["customName"]); value != "" {
		return value
	}
	return nestedString(item, "attributes", "customName")
}

func baseNameOf(item map[string]any) string {
	if value := stringValue(item["name"]); value != "" {
		return value
	}
	if info, ok := item["info"].(map[string]any); ok {
		if value := stringValue(info["name"]); value != "" {
			return value
		}
	}
	if value := customNameOf(item); value != "" {
		return value
	}
	return idOf(item)
}

func nameOf(item map[string]any) string {
	if value := customNameOf(item); value != "" {
		return value
	}
	if value := stringValue(item["name"]); value != "" {
		return value
	}
	if value := nestedString(item, "attributes", "name"); value != "" {
		return value
	}
	if info, ok := item["info"].(map[string]any); ok {
		if value := stringValue(info["name"]); value != "" {
			return value
		}
	}
	return idOf(item)
}

func displayDeviceName(item config.CachedItem) string {
	if strings.TrimSpace(item.CustomName) != "" {
		return item.CustomName
	}
	if strings.TrimSpace(item.Name) != "" {
		return item.Name
	}
	return item.ID
}

func isDukaOneDevice(item map[string]any) bool {
	return stringValue(item["source"]) == "dukaone" || dukaone.IsQualifiedID(idOf(item))
}

func nestedString(item map[string]any, first, second string) string {
	nested, ok := item[first].(map[string]any)
	if !ok {
		return ""
	}
	return stringValue(nested[second])
}

func mapLookup(item map[string]any, keys ...string) any {
	var current any = item
	for _, key := range keys {
		next, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = next[key]
	}
	return current
}

func stringValue(values ...any) string {
	for _, value := range values {
		if value == nil {
			continue
		}
		switch v := value.(type) {
		case string:
			if v != "" {
				return v
			}
		case fmt.Stringer:
			s := v.String()
			if s != "" {
				return s
			}
		case bool:
			return strconv.FormatBool(v)
		case int:
			return strconv.Itoa(v)
		case int8, int16, int32, int64:
			return fmt.Sprint(v)
		case uint, uint8, uint16, uint32, uint64:
			return fmt.Sprint(v)
		case float32, float64:
			return fmt.Sprint(v)
		default:
			s := fmt.Sprint(v)
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func parseScalar(raw string) any {
	if raw == "true" {
		return true
	}
	if raw == "false" {
		return false
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil && strings.Contains(raw, ".") {
		return f
	}
	return raw
}

func consumeBoolFlag(args []string, flag string, target *bool) []string {
	filtered := args[:0]
	for _, arg := range args {
		if arg == flag {
			*target = true
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

func consumeValueFlag(args []string, flag string, target *string) []string {
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == flag && i+1 < len(args) {
			*target = args[i+1]
			i++
			continue
		}
		if value, ok := strings.CutPrefix(arg, flag+"="); ok {
			*target = value
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

func probeHub(ctx context.Context, host string, stdout io.Writer) error {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "8443"))
	if err != nil {
		return fmt.Errorf("could not reach %s:8443: %w", host, err)
	}
	defer conn.Close()
	_, err = fmt.Fprintf(stdout, "reachable: %s:8443\n", host)
	return err
}

func requireReachableHub(ctx context.Context, host string) error {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, "8443"))
	if err != nil {
		return fmt.Errorf("hub %s:8443 is not reachable; `hub list` shows configured hubs, use `hubhq hub discover` for a live scan", host)
	}
	_ = conn.Close()
	return nil
}

const defaultSpeakerSound = "sound-0"

func parseSpeakerSoundName(value string) (string, bool) {
	normalized := normalizeSpeakerSound(value)
	if normalized == "" {
		return "", false
	}
	return normalized, true
}

func normalizeSpeakerSound(value string) string {
	normalized := strings.TrimSpace(strings.ToLower(value))
	switch normalized {
	case "", "test", "sound", "sound-0", "speaker sound-0", "speaker test":
		return defaultSpeakerSound
	case "sound-1", "speaker sound-1":
		return "sound-1"
	case "sound-2", "speaker sound-2":
		return "sound-2"
	case "sound-3", "speaker sound-3":
		return "sound-3"
	case "sound-4", "speaker sound-4":
		return "sound-4"
	case "sound-5", "speaker sound-5":
		return "sound-5"
	default:
		return ""
	}
}

func playLocalSpeakerSound(ctx context.Context, stdout io.Writer, sound string) error {
	sound = normalizeSpeakerSound(sound)
	if sound == "" {
		sound = defaultSpeakerSound
	}
	commands, err := localSoundCommands(sound)
	if err != nil {
		return err
	}
	var lastErr error
	for _, candidate := range commands {
		cmd := exec.CommandContext(ctx, candidate.tool, candidate.args...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err == nil {
			_, err = fmt.Fprintf(stdout, "played %s on local computer speaker\n", sound)
			return err
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("play local speaker sound %s: %w", sound, lastErr)
}

func playLocalMedia(ctx context.Context, source string) error {
	sourceFile, cleanup, err := loadLocalPlaybackFile(ctx, source)
	if err != nil {
		return err
	}
	defer cleanup()

	commands, err := localMediaCommands(sourceFile)
	if err != nil {
		return err
	}
	var lastErr error
	for _, candidate := range commands {
		cmd := exec.CommandContext(ctx, candidate.tool, candidate.args...)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return fmt.Errorf("play local media: %w", lastErr)
}

func generatedSpeakerSoundFile(sound string) (string, error) {
	sound = normalizeSpeakerSound(sound)
	if sound == "" {
		sound = defaultSpeakerSound
	}
	file, err := os.CreateTemp(os.TempDir(), "dirigera-"+sound+"-*.wav")
	if err != nil {
		return "", fmt.Errorf("create temp speaker sound path: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := writeSpeakerSoundWAV(path, sound); err != nil {
		return "", err
	}
	return path, nil
}

func writeSpeakerSoundWAV(path, sound string) error {
	const (
		sampleRate    = 22050
		bitsPerSample = 16
		channels      = 1
	)

	profile := speakerSoundProfile(sound)
	sampleCount := sampleRate * profile.DurationMS / 1000
	dataSize := sampleCount * channels * (bitsPerSample / 8)
	byteRate := sampleRate * channels * (bitsPerSample / 8)
	blockAlign := channels * (bitsPerSample / 8)

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create test tone file: %w", err)
	}
	defer file.Close()

	writeString := func(value string) error {
		_, err := file.WriteString(value)
		return err
	}
	writeLE := func(value any) error {
		return binary.Write(file, binary.LittleEndian, value)
	}

	if err := writeString("RIFF"); err != nil {
		return fmt.Errorf("write wav header: %w", err)
	}
	if err := writeLE(uint32(36 + dataSize)); err != nil {
		return fmt.Errorf("write wav size: %w", err)
	}
	if err := writeString("WAVEfmt "); err != nil {
		return fmt.Errorf("write wav format header: %w", err)
	}
	if err := writeLE(uint32(16)); err != nil {
		return fmt.Errorf("write wav fmt chunk size: %w", err)
	}
	if err := writeLE(uint16(1)); err != nil {
		return fmt.Errorf("write wav audio format: %w", err)
	}
	if err := writeLE(uint16(channels)); err != nil {
		return fmt.Errorf("write wav channels: %w", err)
	}
	if err := writeLE(uint32(sampleRate)); err != nil {
		return fmt.Errorf("write wav sample rate: %w", err)
	}
	if err := writeLE(uint32(byteRate)); err != nil {
		return fmt.Errorf("write wav byte rate: %w", err)
	}
	if err := writeLE(uint16(blockAlign)); err != nil {
		return fmt.Errorf("write wav block align: %w", err)
	}
	if err := writeLE(uint16(bitsPerSample)); err != nil {
		return fmt.Errorf("write wav bits per sample: %w", err)
	}
	if err := writeString("data"); err != nil {
		return fmt.Errorf("write wav data header: %w", err)
	}
	if err := writeLE(uint32(dataSize)); err != nil {
		return fmt.Errorf("write wav data size: %w", err)
	}

	for i := 0; i < sampleCount; i++ {
		t := float64(i) / sampleRate
		sample := synthSpeakerSoundSample(profile, t, i, sampleRate, sampleCount)
		value := int16(sample * math.MaxInt16)
		if err := writeLE(value); err != nil {
			return fmt.Errorf("write wav sample: %w", err)
		}
	}
	return nil
}

type localPlaybackCommand struct {
	tool string
	args []string
}

type speakerSoundTone struct {
	StartMS    int
	DurationMS int
	Frequency  float64
	Amplitude  float64
}

type speakerSoundSpec struct {
	DurationMS int
	Tones      []speakerSoundTone
}

func speakerSoundProfile(sound string) speakerSoundSpec {
	switch normalizeSpeakerSound(sound) {
	case "sound-1":
		return speakerSoundSpec{DurationMS: 560, Tones: []speakerSoundTone{
			{StartMS: 0, DurationMS: 170, Frequency: 392.00, Amplitude: 0.21},
			{StartMS: 180, DurationMS: 190, Frequency: 523.25, Amplitude: 0.19},
		}}
	case "sound-2":
		return speakerSoundSpec{DurationMS: 560, Tones: []speakerSoundTone{
			{StartMS: 0, DurationMS: 190, Frequency: 659.25, Amplitude: 0.18},
			{StartMS: 185, DurationMS: 220, Frequency: 493.88, Amplitude: 0.17},
		}}
	case "sound-3":
		return speakerSoundSpec{DurationMS: 520, Tones: []speakerSoundTone{
			{StartMS: 0, DurationMS: 120, Frequency: 440.00, Amplitude: 0.20},
			{StartMS: 170, DurationMS: 130, Frequency: 554.37, Amplitude: 0.18},
			{StartMS: 330, DurationMS: 140, Frequency: 440.00, Amplitude: 0.17},
		}}
	case "sound-4":
		return speakerSoundSpec{DurationMS: 640, Tones: []speakerSoundTone{
			{StartMS: 0, DurationMS: 160, Frequency: 349.23, Amplitude: 0.18},
			{StartMS: 150, DurationMS: 180, Frequency: 440.00, Amplitude: 0.18},
			{StartMS: 310, DurationMS: 210, Frequency: 523.25, Amplitude: 0.17},
		}}
	case "sound-5":
		return speakerSoundSpec{DurationMS: 620, Tones: []speakerSoundTone{
			{StartMS: 0, DurationMS: 140, Frequency: 523.25, Amplitude: 0.19},
			{StartMS: 150, DurationMS: 140, Frequency: 659.25, Amplitude: 0.17},
			{StartMS: 310, DurationMS: 170, Frequency: 587.33, Amplitude: 0.17},
		}}
	default:
		return speakerSoundSpec{DurationMS: 500, Tones: []speakerSoundTone{
			{StartMS: 0, DurationMS: 180, Frequency: 523.25, Amplitude: 0.20},
			{StartMS: 185, DurationMS: 190, Frequency: 659.25, Amplitude: 0.16},
		}}
	}
}

func synthSpeakerSoundSample(profile speakerSoundSpec, t float64, sampleIndex, sampleRate, sampleCount int) float64 {
	var sample float64
	ms := t * 1000.0
	for _, tone := range profile.Tones {
		start := float64(tone.StartMS)
		end := float64(tone.StartMS + tone.DurationMS)
		if ms < start || ms > end {
			continue
		}
		localT := (ms - start) / 1000.0
		phase := 2 * math.Pi * tone.Frequency * localT
		toneSample := math.Sin(phase) + 0.22*math.Sin(2*phase) + 0.08*math.Sin(3*phase)
		envelope := 1.0
		attackMS := math.Min(28.0, float64(tone.DurationMS)*0.2)
		releaseMS := math.Min(90.0, float64(tone.DurationMS)*0.35)
		if attackMS > 0 && ms < start+attackMS {
			envelope *= (ms - start) / attackMS
		}
		if releaseMS > 0 && ms > end-releaseMS {
			envelope *= (end - ms) / releaseMS
		}
		if envelope < 0 {
			envelope = 0
		}
		sample += toneSample * tone.Amplitude * envelope
	}
	globalAttack := float64(sampleRate) * 0.004
	globalRelease := float64(sampleRate) * 0.01
	global := 1.0
	if float64(sampleIndex) < globalAttack {
		global = float64(sampleIndex) / globalAttack
	}
	if remaining := sampleCount - sampleIndex; float64(remaining) < globalRelease {
		global *= float64(remaining) / globalRelease
	}
	if sample > 0.82 {
		sample = 0.82
	}
	if sample < -0.82 {
		sample = -0.82
	}
	return sample * global
}

func hubSpeakerSoundProfile(sound string) (string, int) {
	switch normalizeSpeakerSound(sound) {
	case "sound-1":
		return audioClipOpened, 18
	case "sound-2":
		return audioClipClosed, 18
	case "sound-3":
		return audioClipOpened, 24
	case "sound-4":
		return audioClipClosed, 24
	case "sound-5":
		return audioClipOpened, 30
	default:
		return audioClipClosed, 20
	}
}

func localSoundCommands(sound string) ([]localPlaybackCommand, error) {
	commands := make([]localPlaybackCommand, 0, 4)
	if testFile, err := generatedSpeakerSoundFile(sound); err == nil {
		if _, err := exec.LookPath("afplay"); err == nil {
			commands = append(commands, localPlaybackCommand{tool: "afplay", args: []string{testFile}})
		}
		if _, err := exec.LookPath("paplay"); err == nil {
			commands = append(commands, localPlaybackCommand{tool: "paplay", args: []string{testFile}})
		}
		if _, err := exec.LookPath("aplay"); err == nil {
			commands = append(commands, localPlaybackCommand{tool: "aplay", args: []string{testFile}})
		}
	}
	if len(commands) == 0 {
		return nil, errors.New("no supported local audio playback tool found")
	}
	return commands, nil
}

func localMediaCommands(sourceFile string) ([]localPlaybackCommand, error) {
	commands := make([]localPlaybackCommand, 0, 3)
	if _, err := exec.LookPath("afplay"); err == nil {
		commands = append(commands, localPlaybackCommand{tool: "afplay", args: []string{sourceFile}})
	}
	if _, err := exec.LookPath("paplay"); err == nil {
		commands = append(commands, localPlaybackCommand{tool: "paplay", args: []string{sourceFile}})
	}
	if _, err := exec.LookPath("aplay"); err == nil {
		commands = append(commands, localPlaybackCommand{tool: "aplay", args: []string{sourceFile}})
	}
	if len(commands) == 0 {
		return nil, errors.New("no supported local audio playback tool found")
	}
	return commands, nil
}

func localSoundCommand() (string, []string, error) {
	commands, err := localSoundCommands(defaultSpeakerSound)
	if err != nil {
		return "", nil, errors.New("no supported local audio playback tool found")
	}
	first := commands[0]
	return first.tool, first.args, nil
}

func looksLikeURL(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func configGetValue(cfg *config.Config, key string) (string, error) {
	switch key {
	case "llm.provider":
		return cfg.LLM.Provider, nil
	case "llm.base_url":
		return cfg.LLM.BaseURL, nil
	case "llm.model":
		return cfg.LLM.Model, nil
	case "llm.api_key":
		return cfg.LLM.APIKey, nil
	case "llm.timeout_seconds":
		return strconv.Itoa(cfg.LLM.TimeoutSeconds), nil
	case "llm.cache_ttl_seconds":
		return strconv.Itoa(cfg.LLM.CacheTTLSeconds), nil
	case "llm.cache_max_entries":
		return strconv.Itoa(cfg.LLM.CacheMaxEntries), nil
	case "apple.airplay_credentials":
		return cfg.Apple.AirPlayCredentials, nil
	case "apple.airplay_password":
		return cfg.Apple.AirPlayPassword, nil
	case "apple.raop_credentials":
		return cfg.Apple.RAOPCredentials, nil
	case "apple.raop_password":
		return cfg.Apple.RAOPPassword, nil
	case "dukaone.backend":
		return cfg.DukaOne.Backend, nil
	case "dukaone.python":
		return cfg.DukaOne.Python, nil
	case "dukaone.password":
		return cfg.DukaOne.Password, nil
	case "location.latitude":
		return strconv.FormatFloat(cfg.Location.Latitude, 'f', -1, 64), nil
	case "location.longitude":
		return strconv.FormatFloat(cfg.Location.Longitude, 'f', -1, 64), nil
	case "location.time_zone":
		return cfg.Location.TimeZone, nil
	case "access.path":
		return strings.TrimSpace(cfg.Access.Path), nil
	default:
		return "", fmt.Errorf("unsupported config key %q", key)
	}
}

func configSetValue(cfg *config.Config, key, value string) error {
	switch key {
	case "llm.provider":
		cfg.LLM.Provider = strings.TrimSpace(value)
		return nil
	case "llm.base_url":
		cfg.LLM.BaseURL = strings.TrimSpace(value)
		return nil
	case "llm.model":
		cfg.LLM.Model = strings.TrimSpace(value)
		return nil
	case "llm.api_key":
		cfg.LLM.APIKey = strings.TrimSpace(value)
		return nil
	case "llm.timeout_seconds":
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("parse llm.timeout_seconds: %w", err)
		}
		cfg.LLM.TimeoutSeconds = parsed
		return nil
	case "llm.cache_ttl_seconds":
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("parse llm.cache_ttl_seconds: %w", err)
		}
		cfg.LLM.CacheTTLSeconds = parsed
		return nil
	case "llm.cache_max_entries":
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("parse llm.cache_max_entries: %w", err)
		}
		cfg.LLM.CacheMaxEntries = parsed
		return nil
	case "apple.airplay_credentials":
		cfg.Apple.AirPlayCredentials = strings.TrimSpace(value)
		return nil
	case "apple.airplay_password":
		cfg.Apple.AirPlayPassword = strings.TrimSpace(value)
		return nil
	case "apple.raop_credentials":
		cfg.Apple.RAOPCredentials = strings.TrimSpace(value)
		return nil
	case "apple.raop_password":
		cfg.Apple.RAOPPassword = strings.TrimSpace(value)
		return nil
	case "dukaone.backend":
		cfg.DukaOne.Backend = strings.TrimSpace(value)
		return nil
	case "dukaone.python":
		cfg.DukaOne.Python = strings.TrimSpace(value)
		return nil
	case "dukaone.password":
		cfg.DukaOne.Password = strings.TrimSpace(value)
		return nil
	case "location.latitude":
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return fmt.Errorf("parse location.latitude: %w", err)
		}
		cfg.Location.Latitude = parsed
		return nil
	case "location.longitude":
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return fmt.Errorf("parse location.longitude: %w", err)
		}
		cfg.Location.Longitude = parsed
		return nil
	case "location.time_zone":
		cfg.Location.TimeZone = strings.TrimSpace(value)
		return nil
	case "access.path":
		cfg.Access.Path = strings.TrimSpace(value)
		return nil
	default:
		return fmt.Errorf("unsupported config key %q", key)
	}
}

func loadLocalPlaybackFile(ctx context.Context, source string) (string, func(), error) {
	raw, err := loadMediaSource(ctx, source)
	if err != nil {
		return "", nil, err
	}
	if _, _, err := decodeWAV(raw); err != nil {
		return "", nil, fmt.Errorf("local playback currently supports WAV audio only: %w", err)
	}
	tmp, err := os.CreateTemp("", "dirigera-local-playback-*.wav")
	if err != nil {
		return "", nil, fmt.Errorf("create temp media file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("write temp media file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("close temp media file: %w", err)
	}
	return tmp.Name(), func() {
		_ = os.Remove(tmp.Name())
	}, nil
}

func loadMediaSource(ctx context.Context, source string) ([]byte, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return nil, errors.New("media source is required")
	}
	if looksLikeURL(source) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, fmt.Errorf("build media request: %w", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch media source: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("fetch media source: unexpected status %s", resp.Status)
		}
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read media source: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return nil, fmt.Errorf("read media source: %w", err)
	}
	return data, nil
}

func resolveSpeakerMediaSource(cfg *config.Config, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", errors.New("media source is required")
	}
	if looksLikeURL(source) {
		return source, nil
	}
	if filepath.IsAbs(source) {
		info, err := os.Stat(source)
		if err != nil {
			return "", fmt.Errorf("stat media source %s: %w", source, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("media source is a directory: %s", source)
		}
		return source, nil
	}
	if info, err := os.Stat(source); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("media source is a directory: %s", source)
		}
		return source, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat media source %s: %w", source, err)
	}
	if err := config.EnsureAccessLayout(cfg); err != nil {
		return "", err
	}
	base, err := config.AccessSoundsDir(cfg)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(source)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("media source escapes access/sounds: %s", source)
	}
	resolved := filepath.Join(base, clean)
	baseClean := filepath.Clean(base)
	resolvedClean := filepath.Clean(resolved)
	if resolvedClean != baseClean && !strings.HasPrefix(resolvedClean, baseClean+string(os.PathSeparator)) {
		return "", fmt.Errorf("media source escapes access/sounds: %s", source)
	}
	info, err := os.Stat(resolvedClean)
	if err != nil {
		return "", fmt.Errorf("stat media source %s: %w", resolvedClean, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("media source is a directory: %s", resolvedClean)
	}
	return resolvedClean, nil
}

type wavFormat struct {
	SampleRate    int
	ChannelCount  int
	BitsPerSample int
}

func decodeWAV(data []byte) (wavFormat, []byte, error) {
	if len(data) < 44 {
		return wavFormat{}, nil, errors.New("wav file too small")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return wavFormat{}, nil, errors.New("invalid wav header")
	}

	var format wavFormat
	var pcm []byte
	offset := 12
	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if offset+chunkSize > len(data) {
			return wavFormat{}, nil, errors.New("invalid wav chunk size")
		}
		chunkData := data[offset : offset+chunkSize]
		switch chunkID {
		case "fmt ":
			if len(chunkData) < 16 {
				return wavFormat{}, nil, errors.New("invalid wav fmt chunk")
			}
			audioFormat := binary.LittleEndian.Uint16(chunkData[0:2])
			if audioFormat != 1 {
				return wavFormat{}, nil, fmt.Errorf("unsupported wav format %d", audioFormat)
			}
			format.ChannelCount = int(binary.LittleEndian.Uint16(chunkData[2:4]))
			format.SampleRate = int(binary.LittleEndian.Uint32(chunkData[4:8]))
			format.BitsPerSample = int(binary.LittleEndian.Uint16(chunkData[14:16]))
		case "data":
			pcm = append([]byte(nil), chunkData...)
		}
		offset += chunkSize
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if format.SampleRate == 0 || format.ChannelCount == 0 || format.BitsPerSample == 0 {
		return wavFormat{}, nil, errors.New("missing wav fmt chunk")
	}
	if len(pcm) == 0 {
		return wavFormat{}, nil, errors.New("missing wav data chunk")
	}
	if format.BitsPerSample != 8 && format.BitsPerSample != 16 {
		return wavFormat{}, nil, fmt.Errorf("unsupported wav bit depth %d", format.BitsPerSample)
	}
	if format.ChannelCount != 1 && format.ChannelCount != 2 {
		return wavFormat{}, nil, fmt.Errorf("unsupported wav channel count %d", format.ChannelCount)
	}
	return format, pcm, nil
}

func sanitizeHostID(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.NewReplacer(":", "-", ".", "-", " ", "-").Replace(host)
	host = strings.Trim(host, "-")
	if host == "" {
		return "hub"
	}
	return host
}
