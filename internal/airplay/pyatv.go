package airplay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jbenner/hubhq/internal/config"
)

func PlayURL(ctx context.Context, device Device, mediaURL string, options config.AppleConfig) error {
	return PlayFile(ctx, device, mediaURL, options)
}

func PlayFile(ctx context.Context, device Device, source string, options config.AppleConfig) error {
	path, err := atvremotePath()
	if err != nil {
		return err
	}
	localFile, cleanup, err := ensureLocalFile(ctx, source)
	if err != nil {
		return err
	}
	defer cleanup()
	return runATVRemote(ctx, path, device, options, "stream_file="+localFile)
}

func atvremotePath() (string, error) {
	candidates := []string{
		filepath.Join(".venv", "bin", "atvremote"),
		"atvremote",
	}
	for _, candidate := range candidates {
		if strings.Contains(candidate, string(filepath.Separator)) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("atvremote not found; install pyatv or create .venv with pyatv")
}

func runATVRemote(ctx context.Context, path string, device Device, options config.AppleConfig, command string) error {
	args := []string{"--scan-hosts", device.Host}
	if device.ID != "" && device.ID != device.Host {
		args = append(args, "--id", device.ID)
	}
	if strings.TrimSpace(options.AirPlayCredentials) != "" {
		args = append(args, "--airplay-credentials", options.AirPlayCredentials)
	}
	if strings.TrimSpace(options.AirPlayPassword) != "" {
		args = append(args, "--airplay-password", options.AirPlayPassword)
	}
	if strings.TrimSpace(options.RAOPCredentials) != "" {
		args = append(args, "--raop-credentials", options.RAOPCredentials)
	}
	if strings.TrimSpace(options.RAOPPassword) != "" {
		args = append(args, "--raop-password", options.RAOPPassword)
	}
	args = append(args, command)
	cmd := exec.CommandContext(ctx, path, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pyatv playback failed: %s", strings.TrimSpace(output.String()))
	}
	return nil
}

func ensureLocalFile(ctx context.Context, source string) (string, func(), error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", nil, fmt.Errorf("media source is required")
	}
	if !looksLikeURL(source) {
		absolutePath, err := filepath.Abs(source)
		if err != nil {
			return "", nil, fmt.Errorf("resolve media path: %w", err)
		}
		return absolutePath, func() {}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return "", nil, fmt.Errorf("build media request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("fetch media source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", nil, fmt.Errorf("fetch media source: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}

	tmp, err := os.CreateTemp("", "dirigera-airplay-*"+filepath.Ext(source))
	if err != nil {
		return "", nil, fmt.Errorf("create temp media file: %w", err)
	}
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		_ = os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("save temp media file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return "", nil, fmt.Errorf("close temp media file: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(tmp.Name())
	}
	return tmp.Name(), cleanup, nil
}

func looksLikeURL(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}
