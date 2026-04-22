package cast

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	castapp "github.com/vishen/go-chromecast/application"
	castdns "github.com/vishen/go-chromecast/dns"
)

type Device struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	UUID  string `json:"uuid"`
}

func Discover(ctx context.Context, timeout time.Duration) ([]Device, error) {
	discoverCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	entries, err := castdns.DiscoverCastDNSEntries(discoverCtx, nil)
	if err != nil {
		return nil, fmt.Errorf("discover cast devices: %w", err)
	}

	seen := map[string]Device{}
	for entry := range entries {
		device := Device{
			ID:    firstNonEmpty(entry.UUID, entry.DeviceName, entry.GetAddr()),
			Name:  firstNonEmpty(entry.DeviceName, entry.Name, entry.GetAddr()),
			Model: entry.Device,
			Host:  entry.GetAddr(),
			Port:  entry.Port,
			UUID:  entry.UUID,
		}
		key := firstNonEmpty(device.UUID, device.Host)
		if _, ok := seen[key]; !ok {
			seen[key] = device
		}
	}

	devices := make([]Device, 0, len(seen))
	for _, device := range seen {
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool {
		if strings.EqualFold(devices[i].Name, devices[j].Name) {
			return devices[i].Host < devices[j].Host
		}
		return strings.ToLower(devices[i].Name) < strings.ToLower(devices[j].Name)
	})
	return devices, nil
}

func ResolveByName(ctx context.Context, timeout time.Duration, query string) (Device, error) {
	devices, err := Discover(ctx, timeout)
	if err != nil {
		return Device{}, err
	}
	device, err := resolve(devices, query)
	if err == nil {
		return device, nil
	}
	if timeout < 8*time.Second {
		devices, retryErr := Discover(ctx, 8*time.Second)
		if retryErr == nil {
			return resolve(devices, query)
		}
	}
	return Device{}, err
}

func PlayFile(ctx context.Context, device Device, filename string, debug bool) error {
	app := castapp.NewApplication(
		castapp.WithDebug(debug),
		castapp.WithCacheDisabled(true),
		castapp.WithDeviceNameOverride(device.Name),
	)
	if err := app.Start(device.Host, device.Port); err != nil {
		return fmt.Errorf("connect to cast device %s: %w", device.Name, err)
	}
	defer func() {
		_ = app.Close(false)
	}()

	loadErrCh := make(chan error, 1)
	go func() {
		if err := app.Load(filename, 0, "", false, false, false); err != nil {
			loadErrCh <- fmt.Errorf("play media on %s: %w", device.Name, err)
			return
		}
		loadErrCh <- nil
	}()

	// Keep the process alive long enough for the cast device to fetch the WAV
	// from the local media server and begin playback. If Load fails quickly,
	// surface that error; otherwise treat the playback as started and hold the
	// session open briefly before returning.
	startupWindow := 2 * time.Second
	playbackGrace := 4 * time.Second

	select {
	case <-ctx.Done():
		_ = app.Close(true)
		return ctx.Err()
	case err := <-loadErrCh:
		if err != nil {
			_ = app.Close(true)
			return err
		}
	case <-time.After(startupWindow):
	}

	select {
	case <-ctx.Done():
		_ = app.Close(true)
		return ctx.Err()
	case err := <-loadErrCh:
		if err != nil {
			_ = app.Close(true)
			return err
		}
		return nil
	case <-time.After(playbackGrace):
		return nil
	}
}

func resolve(devices []Device, query string) (Device, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		if len(devices) == 1 {
			return devices[0], nil
		}
		if len(devices) == 0 {
			return Device{}, fmt.Errorf("no cast devices found")
		}
		return Device{}, fmt.Errorf("multiple cast devices found; specify a name")
	}

	normalizedQuery := normalize(query)
	var exact []Device
	for _, device := range devices {
		if normalize(device.ID) == normalizedQuery || normalize(device.Name) == normalizedQuery || normalize(device.UUID) == normalizedQuery {
			exact = append(exact, device)
		}
	}
	if len(exact) == 1 {
		return exact[0], nil
	}
	if len(exact) > 1 {
		return Device{}, fmt.Errorf("ambiguous cast device match for %q", query)
	}

	var partial []Device
	for _, device := range devices {
		if strings.Contains(normalize(device.Name), normalizedQuery) {
			partial = append(partial, device)
		}
	}
	if len(partial) == 1 {
		return partial[0], nil
	}
	if len(partial) > 1 {
		return Device{}, fmt.Errorf("ambiguous cast device match for %q", query)
	}
	return Device{}, fmt.Errorf("no cast device matched %q", query)
}

func normalize(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	lastSpace := false
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			b.WriteRune(r)
			lastSpace = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.':
			if !lastSpace && b.Len() > 0 {
				b.WriteByte(' ')
				lastSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
