package discovery

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const serviceType = "_ihsp._tcp"
const defaultPort = 8443

type Hub struct {
	ID   string
	Name string
	Host string
	Port int
}

func Find(ctx context.Context, timeout time.Duration) ([]Hub, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	var mdnsErr error
	switch runtime.GOOS {
	case "darwin":
		hubs, err := findWithDNSSD(ctx, timeout)
		if err == nil && len(hubs) > 0 {
			return hubs, nil
		}
		mdnsErr = err
	case "linux":
		hubs, err := findWithAvahi(ctx, timeout)
		if err == nil && len(hubs) > 0 {
			return hubs, nil
		}
		mdnsErr = err
	default:
		mdnsErr = fmt.Errorf("unsupported OS for automatic discovery: %s", runtime.GOOS)
	}

	scanTimeout := timeout
	if scanTimeout < 8*time.Second {
		scanTimeout = 8 * time.Second
	}
	hubs, err := findWithSubnetScan(ctx, scanTimeout)
	if err == nil && len(hubs) > 0 {
		return hubs, nil
	}
	if mdnsErr != nil {
		return nil, fmt.Errorf("%v; subnet scan fallback also failed: %w", mdnsErr, err)
	}
	return nil, err
}

func findWithDNSSD(ctx context.Context, timeout time.Duration) ([]Hub, error) {
	browseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(browseCtx, "dns-sd", "-B", serviceType, "local.")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	names := map[string]struct{}{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, serviceType) {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				instance := fields[len(fields)-1]
				if instance != serviceType {
					names[instance] = struct{}{}
				}
			}
		}
	}
	_ = cmd.Wait()

	var hubs []Hub
	for name := range names {
		resolveCtx, resolveCancel := context.WithTimeout(ctx, 3*time.Second)
		hub, err := resolveWithDNSSD(resolveCtx, name)
		resolveCancel()
		if err == nil {
			hubs = append(hubs, hub)
		}
	}
	slices.SortFunc(hubs, func(a, b Hub) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	if len(hubs) == 0 {
		return nil, errors.New("no DIRIGERA hub discovered via mDNS")
	}
	return hubs, nil
}

func resolveWithDNSSD(ctx context.Context, instance string) (Hub, error) {
	cmd := exec.CommandContext(ctx, "dns-sd", "-L", instance, serviceType, "local.")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Hub{}, err
	}
	if err := cmd.Start(); err != nil {
		return Hub{}, err
	}

	hub := Hub{Name: instance}
	srvRe := regexp.MustCompile(`can be reached at ([^ ]+)\.:([0-9]+)`)
	txtRe := regexp.MustCompile(`uuid=([^ ]+)`)
	ipRe := regexp.MustCompile(`ipv4address=([^ ]+)`)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if m := srvRe.FindStringSubmatch(line); len(m) == 3 {
			hub.Host = strings.TrimSuffix(m[1], ".local")
			port, _ := strconv.Atoi(m[2])
			hub.Port = port
		}
		if m := txtRe.FindStringSubmatch(line); len(m) == 2 {
			hub.ID = m[1]
		}
		if m := ipRe.FindStringSubmatch(line); len(m) == 2 {
			hub.Host = m[1]
		}
	}
	_ = cmd.Wait()

	if hub.ID == "" {
		hub.ID = sanitizeID(instance)
	}
	if hub.Host == "" {
		return Hub{}, fmt.Errorf("could not resolve host for %s", instance)
	}
	if hub.Port == 0 {
		hub.Port = defaultPort
	}
	return hub, nil
}

func findWithAvahi(ctx context.Context, timeout time.Duration) ([]Hub, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "avahi-browse", serviceType, "-trp")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("avahi-browse failed: %w", err)
	}
	var hubs []Hub
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, "=") {
			continue
		}
		parts := strings.Split(line, ";")
		if len(parts) < 9 {
			continue
		}
		port, _ := strconv.Atoi(parts[8])
		hubs = append(hubs, Hub{
			ID:   sanitizeID(parts[3]),
			Name: parts[3],
			Host: parts[7],
			Port: port,
		})
	}
	if len(hubs) == 0 {
		return nil, errors.New("no DIRIGERA hub discovered via mDNS")
	}
	return hubs, nil
}

func sanitizeID(value string) string {
	value = strings.ToLower(value)
	value = strings.ReplaceAll(value, " ", "-")
	value = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(value, "")
	if value == "" {
		return "hub"
	}
	return value
}

func findWithSubnetScan(ctx context.Context, timeout time.Duration) ([]Hub, error) {
	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	targets, err := scanTargets()
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, errors.New("no private IPv4 subnets available for fallback scan")
	}

	candidates := make(chan string)
	results := make(chan Hub, 16)
	var wg sync.WaitGroup

	workers := 64
	if len(targets) < workers {
		workers = len(targets)
	}
	if workers == 0 {
		workers = 1
	}

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for host := range candidates {
				hub, ok := probeHost(scanCtx, host)
				if !ok {
					continue
				}
				select {
				case results <- hub:
				case <-scanCtx.Done():
					return
				}
			}
		}()
	}

	go func() {
		defer close(candidates)
		for _, host := range targets {
			select {
			case candidates <- host:
			case <-scanCtx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(results)
	}()

	hubsByID := map[string]Hub{}
	for hub := range results {
		hubsByID[hub.ID] = hub
	}

	hubs := make([]Hub, 0, len(hubsByID))
	for _, hub := range hubsByID {
		hubs = append(hubs, hub)
	}
	slices.SortFunc(hubs, func(a, b Hub) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	if len(hubs) == 0 {
		return nil, errors.New("no DIRIGERA hub discovered during subnet scan")
	}
	return hubs, nil
}

func scanTargets() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	seen := map[string]struct{}{}
	var hosts []string
	for _, host := range arpTargets() {
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !isPrivateIPv4(ip) {
				continue
			}
			for _, host := range subnetHosts(ip, ipNet.Mask) {
				if _, exists := seen[host]; exists {
					continue
				}
				seen[host] = struct{}{}
				hosts = append(hosts, host)
			}
		}
	}
	slices.Sort(hosts)
	return hosts, nil
}

func subnetHosts(ip net.IP, mask net.IPMask) []string {
	maskSize, bits := mask.Size()
	if bits != 32 {
		return nil
	}

	network := ip.Mask(mask)
	var hosts []string
	if maskSize >= 24 {
		base := make(net.IP, len(network))
		copy(base, network)
		for i := 1; i < 255; i++ {
			candidate := make(net.IP, len(base))
			copy(candidate, base)
			candidate[3] = byte(i)
			if candidate.Equal(ip) {
				continue
			}
			hosts = append(hosts, candidate.String())
		}
		return hosts
	}

	// For larger private subnets, scan nearby /24 segments around the local address.
	startThirdOctet := maxInt(0, int(ip[2])-2)
	endThirdOctet := minInt(255, int(ip[2])+2)
	for third := startThirdOctet; third <= endThirdOctet; third++ {
		base := net.IPv4(network[0], network[1], byte(third), 0).To4()
		if base == nil {
			continue
		}
		for i := 1; i < 255; i++ {
			candidate := make(net.IP, len(base))
			copy(candidate, base)
			candidate[3] = byte(i)
			if candidate.Equal(ip) {
				continue
			}
			hosts = append(hosts, candidate.String())
		}
	}
	return hosts
}

func isPrivateIPv4(ip net.IP) bool {
	if ip == nil {
		return false
	}
	switch {
	case ip[0] == 10:
		return true
	case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
		return true
	case ip[0] == 192 && ip[1] == 168:
		return true
	default:
		return false
	}
}

func probeHost(ctx context.Context, host string) (Hub, bool) {
	dialer := net.Dialer{Timeout: 300 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(defaultPort)))
	if err != nil {
		return Hub{}, false
	}
	_ = conn.Close()

	probeCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()

	client := &http.Client{
		Timeout: 1200 * time.Millisecond,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	for _, url := range probeURLs(host) {
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if !looksLikeDirigera(resp.StatusCode, body, resp.Header) {
			continue
		}
		return Hub{
			ID:   sanitizeID(host),
			Name: host,
			Host: host,
			Port: defaultPort,
		}, true
	}
	return Hub{}, false
}

func looksLikeDirigera(status int, body []byte, headers http.Header) bool {
	text := strings.ToLower(string(body))
	if strings.Contains(text, "homesmart") || strings.Contains(text, "dirigera") || strings.Contains(text, "access token") {
		return true
	}
	server := strings.ToLower(headers.Get("Server"))
	if strings.Contains(server, "nginx") || strings.Contains(server, "envoy") {
		return true
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest || status == http.StatusNotFound {
		return true
	}
	return false
}

func probeURLs(host string) []string {
	return []string{
		fmt.Sprintf("https://%s:%d/v1/oauth/authorize?audience=homesmart.local&response_type=code&code_challenge=x&code_challenge_method=S256", host, defaultPort),
		fmt.Sprintf("https://%s:%d/v1/devices", host, defaultPort),
		fmt.Sprintf("https://%s:%d/v1", host, defaultPort),
	}
}

func arpTargets() []string {
	cmd := exec.Command("arp", "-a")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`\(([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)\)`)
	matches := re.FindAllStringSubmatch(string(out), -1)
	seen := map[string]struct{}{}
	var hosts []string
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		host := match[1]
		ip := net.ParseIP(host).To4()
		if ip == nil || !isPrivateIPv4(ip) {
			continue
		}
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
