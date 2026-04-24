package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

const (
	clusterServiceType     = "_hubhq._tcp"
	clusterServiceDomain    = "local"
	clusterRefreshEvery     = 2 * time.Second
	clusterQueryListenTimeout = 2500 * time.Millisecond
)

type clusterInstanceView struct {
	Info   runtimeInstanceInfo
	SeenAt time.Time
}

type clusterPeer struct {
	info   runtimeInstanceInfo
	seenAt time.Time
}

type clusterRegistry struct {
	mu      sync.RWMutex
	self    runtimeInstanceInfo
	peers   map[string]clusterPeer
	service *zeroconf.Server
	done    chan struct{}
}

func newClusterRegistry(ctx context.Context, self runtimeInstanceInfo) (*clusterRegistry, error) {
	registry := &clusterRegistry{
		self:  self,
		peers: map[string]clusterPeer{},
		done:  make(chan struct{}),
	}
	if err := registry.register(ctx); err != nil {
		return nil, err
	}
	go registry.refreshLoop()
	if _, err := registry.Refresh(context.Background(), clusterQueryListenTimeout); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return registry, nil
}

func (r *clusterRegistry) Close() error {
	if r == nil {
		return nil
	}
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	if r.service != nil {
		r.service.Shutdown()
	}
	return nil
}

func (r *clusterRegistry) register(ctx context.Context) error {
	if r == nil {
		return nil
	}
	u, err := neturl.Parse(r.self.StatusURL)
	if err != nil {
		return fmt.Errorf("parse status url: %w", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return fmt.Errorf("parse status listen address: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("parse status port: %w", err)
	}
	ips := []string{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip.String())
	}
	instance := sanitizeHostID(firstNonEmpty(r.self.HubKey, r.self.Host, r.self.ID, r.self.Mode)) + "-" + strconv.Itoa(r.self.PID)
	text := []string{
		"mode=" + r.self.Mode,
		"pid=" + strconv.Itoa(r.self.PID),
		"host=" + r.self.Host,
		"hub_id=" + r.self.HubID,
		"hub_name=" + r.self.HubName,
		"hub_host=" + r.self.HubHost,
		"hub_key=" + r.self.HubKey,
		"status_url=" + r.self.StatusURL,
		"started_at=" + r.self.StartedAt.Format(time.RFC3339Nano),
	}
	service, err := zeroconf.RegisterProxy(instance, clusterServiceType, clusterServiceDomain, port, host, ips, text, nil)
	if err != nil {
		return fmt.Errorf("register cluster service: %w", err)
	}
	r.service = service
	go func() {
		<-ctx.Done()
		r.service.Shutdown()
	}()
	return nil
}

func (r *clusterRegistry) refreshLoop() {
	ticker := time.NewTicker(clusterRefreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			_, _ = r.Refresh(context.Background(), clusterQueryListenTimeout)
		}
	}
}

func (r *clusterRegistry) Refresh(ctx context.Context, timeout time.Duration) ([]clusterInstanceView, error) {
	views, err := queryClusterInstances(ctx, timeout)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	for _, view := range views {
		r.peers[view.Info.ID] = clusterPeer{info: view.Info, seenAt: view.SeenAt}
	}
	r.mu.Unlock()
	return views, nil
}

func (r *clusterRegistry) IsLeader(mode string) bool {
	if r == nil {
		return true
	}
	leader := r.leader(mode)
	return leader.ID == "" || leader.ID == r.self.ID
}

func (r *clusterRegistry) LeaderName(mode string) string {
	if r == nil {
		return ""
	}
	leader := r.leader(mode)
	return firstNonEmpty(leader.Host, leader.HubHost, leader.ID)
}

func (r *clusterRegistry) leader(mode string) runtimeInstanceInfo {
	views := r.snapshot()
	candidates := make([]runtimeInstanceInfo, 0, len(views)+1)
	group := clusterGroupKey(r.self)
	if strings.EqualFold(r.self.Mode, mode) {
		candidates = append(candidates, r.self)
	}
	for _, view := range views {
		if !strings.EqualFold(view.Info.Mode, mode) {
			continue
		}
		if clusterGroupKey(view.Info) != group {
			continue
		}
		if !clusterInstanceActive(view) {
			continue
		}
		candidates = append(candidates, view.Info)
	}
	if len(candidates) == 0 {
		return runtimeInstanceInfo{}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].StartedAt.Equal(candidates[j].StartedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].StartedAt.Before(candidates[j].StartedAt)
	})
	return candidates[0]
}

func (r *clusterRegistry) snapshot() []clusterInstanceView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	views := make([]clusterInstanceView, 0, len(r.peers))
	for _, peer := range r.peers {
		views = append(views, clusterInstanceView{Info: peer.info, SeenAt: peer.seenAt})
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Info.Mode != views[j].Info.Mode {
			return views[i].Info.Mode < views[j].Info.Mode
		}
		if views[i].Info.Host != views[j].Info.Host {
			return views[i].Info.Host < views[j].Info.Host
		}
		return views[i].Info.ID < views[j].Info.ID
	})
	return views
}

func queryClusterInstances(ctx context.Context, timeout time.Duration) ([]clusterInstanceView, error) {
	if timeout <= 0 {
		timeout = clusterQueryListenTimeout
	}
	discoverCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4))
	if err != nil {
		return nil, fmt.Errorf("create cluster resolver: %w", err)
	}
	ch := make(chan *zeroconf.ServiceEntry, 32)
	if err := resolver.Browse(discoverCtx, clusterServiceType, clusterServiceDomain, ch); err != nil {
		return nil, fmt.Errorf("browse cluster services: %w", err)
	}
	views := map[string]clusterInstanceView{}
	merge := func(info runtimeInstanceInfo) {
		key := clusterInstanceKey(info)
		if key == "" {
			return
		}
		views[key] = clusterInstanceView{Info: info, SeenAt: time.Now().UTC()}
	}
	for {
		select {
		case <-discoverCtx.Done():
			localViews, _ := localRuntimeInstances(ctx, timeout)
			for _, view := range localViews {
				merge(view.Info)
			}
			out := make([]clusterInstanceView, 0, len(views))
			for _, view := range views {
				out = append(out, view)
			}
			sort.Slice(out, func(i, j int) bool {
				if out[i].Info.Mode != out[j].Info.Mode {
					return out[i].Info.Mode < out[j].Info.Mode
				}
				if out[i].Info.Host != out[j].Info.Host {
					return out[i].Info.Host < out[j].Info.Host
				}
				return out[i].Info.ID < out[j].Info.ID
			})
			return out, nil
		case entry, ok := <-ch:
			if !ok || entry == nil {
				continue
			}
			info := clusterInstanceInfoFromEntry(entry)
			if info.ID == "" {
				continue
			}
			merge(info)
		}
	}
}

func localRuntimeInstances(ctx context.Context, timeout time.Duration) ([]clusterInstanceView, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, "ss", "-ltnp").Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	pidRe := regexp.MustCompile(`pid=([0-9]+)`)
	ports := map[int]struct{}{}
	pids := map[int]int{}
	for _, line := range lines {
		if !strings.Contains(line, "LISTEN") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		_, portStr, err := net.SplitHostPort(fields[3])
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 8080 || port > 8090 {
			continue
		}
		match := pidRe.FindStringSubmatch(line)
		if len(match) != 2 {
			continue
		}
		pid, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		ports[port] = struct{}{}
		pids[port] = pid
	}
	if len(ports) == 0 {
		return nil, nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	probeHosts := []string{}
	if host := preferredRuntimeStatusHost(); strings.TrimSpace(host) != "" {
		probeHosts = append(probeHosts, host)
	}
	probeHosts = append(probeHosts, "127.0.0.1")
	views := make([]clusterInstanceView, 0, len(ports))
	for port := range ports {
		info, err := fetchLocalRuntimeInstance(client, probeHosts, port, pids[port])
		if err != nil {
			continue
		}
		views = append(views, clusterInstanceView{Info: info, SeenAt: time.Now().UTC()})
	}
	return views, nil
}

func fetchLocalRuntimeInstance(client *http.Client, hosts []string, port int, pid int) (runtimeInstanceInfo, error) {
	var lastErr error
	for _, host := range hosts {
		resp, err := client.Get("http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/status")
		if err != nil {
			lastErr = err
			continue
		}
		var payload struct {
			Mode      string         `json:"mode"`
			StartedAt time.Time      `json:"started_at"`
			URL       string         `json:"url"`
			Summary   map[string]any `json:"summary"`
		}
		err = json.NewDecoder(resp.Body).Decode(&payload)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("status endpoint returned %s", resp.Status)
			continue
		}
		info := runtimeInstanceInfo{
			ID:        fmt.Sprintf("local-%d-%d", pid, port),
			Mode:      firstNonEmpty(payload.Mode, "auto-run"),
			PID:       pid,
			Host:      host,
			StartedAt: payload.StartedAt,
			StatusURL: payload.URL,
		}
		if payload.Summary != nil {
			if v, ok := payload.Summary["hub_id"].(string); ok {
				info.HubID = v
			}
			if v, ok := payload.Summary["hub_name"].(string); ok {
				info.HubName = v
			}
			if v, ok := payload.Summary["hub_host"].(string); ok {
				info.HubHost = v
			}
			if v, ok := payload.Summary["hub_key"].(string); ok {
				info.HubKey = v
			}
		}
		if info.StatusURL == "" {
			info.StatusURL = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
		}
		return info, nil
	}
	if lastErr != nil {
		return runtimeInstanceInfo{}, lastErr
	}
	return runtimeInstanceInfo{}, fmt.Errorf("status endpoint unavailable on port %d", port)
}

func clusterInstanceInfoFromEntry(entry *zeroconf.ServiceEntry) runtimeInstanceInfo {
	if entry == nil {
		return runtimeInstanceInfo{}
	}
	values := parseClusterTXT(entry.Text)
	host := firstNonEmpty(firstIPv4(entry.AddrIPv4), entry.HostName)
	statusURL := firstNonEmpty(values["status_url"], "")
	if statusURL == "" && host != "" && entry.Port > 0 {
		statusURL = "http://" + net.JoinHostPort(host, strconv.Itoa(entry.Port))
	}
	pid, _ := strconv.Atoi(values["pid"])
	startedAt, _ := time.Parse(time.RFC3339Nano, values["started_at"])
	if startedAt.IsZero() {
		startedAt, _ = time.Parse(time.RFC3339, values["started_at"])
	}
	info := runtimeInstanceInfo{
		ID:        firstNonEmpty(values["id"], values["hub_key"], entry.Instance, host),
		Mode:      firstNonEmpty(values["mode"], "auto-run"),
		PID:       pid,
		Host:      firstNonEmpty(values["host"], host),
		StartedAt: startedAt,
		HubID:     values["hub_id"],
		HubName:   values["hub_name"],
		HubHost:   values["hub_host"],
		HubKey:    firstNonEmpty(values["hub_key"], values["hub_id"], values["hub_host"]),
		StatusURL: statusURL,
	}
	return info
}

func clusterInstanceKey(info runtimeInstanceInfo) string {
	if strings.TrimSpace(info.StatusURL) != "" {
		return strings.TrimSpace(strings.ToLower(info.StatusURL))
	}
	if strings.TrimSpace(info.ID) != "" {
		return strings.TrimSpace(strings.ToLower(info.ID))
	}
	if info.PID > 0 {
		return fmt.Sprintf("%s-%d-%s", strings.ToLower(info.Mode), info.PID, strings.ToLower(firstNonEmpty(info.Host, info.HubHost)))
	}
	return ""
}

func parseClusterTXT(values []string) map[string]string {
	out := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(val)
	}
	return out
}

func firstIPv4(values []net.IP) string {
	if len(values) == 0 || values[0] == nil {
		return ""
	}
	return values[0].String()
}

func clusterLeaderByGroup(instances []clusterInstanceView) map[string]string {
	grouped := map[string][]clusterInstanceView{}
	for _, instance := range instances {
		if !clusterInstanceActive(instance) {
			continue
		}
		group := clusterGroupKey(instance.Info)
		grouped[group] = append(grouped[group], instance)
	}
	leaders := map[string]string{}
	for group, items := range grouped {
		sort.Slice(items, func(i, j int) bool {
			if items[i].Info.StartedAt.Equal(items[j].Info.StartedAt) {
				return items[i].Info.ID < items[j].Info.ID
			}
			return items[i].Info.StartedAt.Before(items[j].Info.StartedAt)
		})
		if len(items) > 0 {
			leaders[group] = items[0].Info.ID
		}
	}
	return leaders
}

func clusterGroupKey(info runtimeInstanceInfo) string {
	return strings.Join([]string{
		strings.ToLower(strings.TrimSpace(info.Mode)),
		strings.ToLower(firstNonEmpty(info.HubKey, info.HubHost)),
	}, "|")
}

func clusterInstanceActive(view clusterInstanceView) bool {
	if view.SeenAt.IsZero() {
		return false
	}
	return time.Since(view.SeenAt) <= 15*time.Second
}

func clusterInstanceStatus(view clusterInstanceView) string {
	if clusterInstanceActive(view) {
		return "running"
	}
	return "stale"
}
