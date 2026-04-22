package airplay

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Model   string `json:"model"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Service string `json:"service,omitempty"`
}

func Discover(ctx context.Context, timeout time.Duration) ([]Device, error) {
	discoverCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	serviceEntries := make(chan *zeroconf.ServiceEntry, 32)
	var wg sync.WaitGroup
	for _, service := range []string{"_airplay._tcp", "_raop._tcp"} {
		wg.Add(1)
		go func(service string) {
			defer wg.Done()
			entries, err := browse(discoverCtx, service)
			if err != nil {
				return
			}
			for _, entry := range entries {
				select {
				case serviceEntries <- entry:
				case <-discoverCtx.Done():
					return
				}
			}
		}(service)
	}
	go func() {
		wg.Wait()
		close(serviceEntries)
	}()

	seen := map[string]Device{}
	for {
		select {
		case <-discoverCtx.Done():
			return sortedDevices(seen), nil
		case entry, ok := <-serviceEntries:
			if !ok {
				return sortedDevices(seen), nil
			}
			device := fromServiceEntry(entry)
			key := firstNonEmpty(normalizeKey(device.Name), device.Host)
			if key == "" {
				continue
			}
			current, ok := seen[key]
			if !ok || shouldReplace(current, device) {
				seen[key] = device
			}
		}
	}
}

func browse(ctx context.Context, service string) ([]*zeroconf.ServiceEntry, error) {
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4))
	if err != nil {
		return nil, fmt.Errorf("discover airplay devices: %w", err)
	}
	ch := make(chan *zeroconf.ServiceEntry, 16)
	if err := resolver.Browse(ctx, service, "local", ch); err != nil {
		return nil, err
	}
	var entries []*zeroconf.ServiceEntry
	for {
		select {
		case <-ctx.Done():
			return entries, nil
		case entry, ok := <-ch:
			if !ok {
				return entries, nil
			}
			if entry != nil {
				entries = append(entries, entry)
			}
		}
	}
}

func fromServiceEntry(entry *zeroconf.ServiceEntry) Device {
	info := parseTXT(entry.Text)
	service := strings.TrimSuffix(entry.Service, ".local")
	name := firstNonEmpty(info["fn"], parseServiceName(entry.Service, entry.Instance), entry.HostName)
	model := firstNonEmpty(info["am"], info["md"])
	host := firstIPv4(entry.AddrIPv4)
	id := firstNonEmpty(info["deviceid"], info["pk"], entry.Instance, host)
	return Device{
		ID:      strings.TrimSpace(id),
		Name:    strings.TrimSpace(name),
		Model:   strings.TrimSpace(model),
		Host:    host,
		Port:    entry.Port,
		Service: service,
	}
}

func parseTXT(values []string) map[string]string {
	out := make(map[string]string, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if ok {
			out[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(val)
		}
	}
	return out
}

func parseServiceName(service, instance string) string {
	if strings.Contains(service, "_raop") {
		return parseRAOPName(instance)
	}
	return strings.TrimSpace(instance)
}

func parseRAOPName(instance string) string {
	if _, after, ok := strings.Cut(instance, "@"); ok {
		return strings.TrimSpace(after)
	}
	return strings.TrimSpace(instance)
}

func firstIPv4(values []net.IP) string {
	if len(values) == 0 || values[0] == nil {
		return ""
	}
	return values[0].String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeKey(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.NewReplacer("\\", "", "_", " ", "-", " ", ".", " ").Replace(value)
	return strings.Join(strings.Fields(value), " ")
}

func shouldReplace(current, next Device) bool {
	if strings.Contains(current.Service, "_airplay") {
		return false
	}
	return strings.Contains(next.Service, "_airplay")
}

func sortedDevices(devices map[string]Device) []Device {
	out := make([]Device, 0, len(devices))
	for _, device := range devices {
		out = append(out, device)
	}
	sort.Slice(out, func(i, j int) bool {
		if strings.EqualFold(out[i].Name, out[j].Name) {
			return out[i].Host < out[j].Host
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}
