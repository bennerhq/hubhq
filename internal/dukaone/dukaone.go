package dukaone

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jbenner/hubhq/internal/config"
	"github.com/jbenner/hubhq/internal/dukaone/native"
)

const (
	IDPrefix         = "dukaone:"
	DeviceType       = "ventilation"
	defaultPythonBin = "python3"
)

//go:embed bridge.py
var bridgeSource string

type bridgeResponse struct {
	DeviceID        string `json:"device_id"`
	IPAddress       string `json:"ip_address,omitempty"`
	ConfiguredHost  string `json:"configured_host,omitempty"`
	Speed           string `json:"speed,omitempty"`
	Mode            string `json:"mode,omitempty"`
	ManualSpeed     *int   `json:"manual_speed,omitempty"`
	IsOn            *bool  `json:"is_on,omitempty"`
	Humidity        *int   `json:"humidity,omitempty"`
	FilterAlarm     *bool  `json:"filter_alarm,omitempty"`
	FilterTimer     *int   `json:"filter_timer,omitempty"`
	FanRPM          *int   `json:"fan_rpm,omitempty"`
	FirmwareVersion string `json:"firmware_version,omitempty"`
	FirmwareDate    string `json:"firmware_date,omitempty"`
	UnitType        *int   `json:"unit_type,omitempty"`
}

type discoveryResponse struct {
	Devices []bridgeResponse `json:"devices"`
}

func QualifiedID(deviceID string) string {
	return IDPrefix + strings.TrimSpace(deviceID)
}

func IsQualifiedID(id string) bool {
	return strings.HasPrefix(strings.TrimSpace(id), IDPrefix)
}

func RawID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), IDPrefix)
}

func List(ctx context.Context, cfg *config.Config) ([]map[string]any, error) {
	if cfg == nil || len(cfg.DukaOne.Devices) == 0 {
		return nil, nil
	}
	items := make([]map[string]any, 0, len(cfg.DukaOne.Devices))
	var lastErr error
	for _, device := range cfg.DukaOne.Devices {
		state, err := readState(ctx, cfg, device)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", device.Name, err)
			continue
		}
		items = append(items, normalize(device, state))
	}
	if len(items) == 0 && lastErr != nil {
		return nil, lastErr
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(nameOf(items[i])) < strings.ToLower(nameOf(items[j]))
	})
	return items, nil
}

func Discover(ctx context.Context, cfg *config.Config, broadcast string, timeoutSeconds float64) ([]map[string]any, error) {
	if useNative(cfg) {
		timeout := 2.0
		if timeoutSeconds > 0 {
			timeout = timeoutSeconds
		}
		states, err := native.New().Discover(ctx, strings.TrimSpace(broadcast), firstNonEmpty(cfg.DukaOne.Password, "1111"), time.Duration(timeout*float64(time.Second)))
		if err != nil {
			return nil, err
		}
		items := make([]map[string]any, 0, len(states))
		for _, state := range states {
			device := config.DukaOneDeviceConfig{
				DeviceID: state.DeviceID,
				Name:     DefaultName(state.DeviceID),
				Host:     firstNonEmpty(state.IPAddress, broadcast),
			}
			items = append(items, normalize(device, stateToBridgeResponse(state)))
		}
		return items, nil
	}
	args := []string{
		"discover",
		"--ip-address", firstNonEmpty(strings.TrimSpace(broadcast), "<broadcast>"),
		"--password", firstNonEmpty(cfg.DukaOne.Password, "1111"),
	}
	if timeoutSeconds > 0 {
		args = append(args, "--timeout", fmt.Sprintf("%.3f", timeoutSeconds))
	}
	output, err := runBridgeRaw(ctx, cfg, args...)
	if err != nil {
		return nil, err
	}
	var response discoveryResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("parse dukaone discovery response: %w", err)
	}
	items := make([]map[string]any, 0, len(response.Devices))
	for _, state := range response.Devices {
		device := config.DukaOneDeviceConfig{
			DeviceID: state.DeviceID,
			Name:     DefaultName(state.DeviceID),
			Host:     firstNonEmpty(state.IPAddress, broadcast),
		}
		items = append(items, normalize(device, state))
	}
	return items, nil
}

func SetAttributes(ctx context.Context, cfg *config.Config, id string, attrs map[string]any) (map[string]any, error) {
	device, _, err := ResolveConfig(cfg, id)
	if err != nil {
		return nil, err
	}
	if useNative(cfg) {
		state, err := native.New().SetAttributes(ctx, device.DeviceID, firstNonEmpty(cfg.DukaOne.Password, "1111"), firstNonEmpty(device.Host, "255.255.255.255"), attrs, 2*time.Second)
		if err != nil {
			return nil, err
		}
		return normalize(device, stateToBridgeResponse(state)), nil
	}
	state, err := runBridge(ctx, cfg, "set", device, attrs)
	if err != nil {
		return nil, err
	}
	return normalize(device, state), nil
}

func readState(ctx context.Context, cfg *config.Config, device config.DukaOneDeviceConfig) (bridgeResponse, error) {
	if useNative(cfg) {
		state, err := native.New().ReadState(ctx, device.DeviceID, firstNonEmpty(cfg.DukaOne.Password, "1111"), firstNonEmpty(device.Host, "255.255.255.255"), 2*time.Second)
		if err != nil {
			return bridgeResponse{}, err
		}
		return stateToBridgeResponse(state), nil
	}
	return runBridge(ctx, cfg, "state", device, nil)
}

func Add(cfg *config.Config, deviceID, name, host, room string) error {
	if cfg == nil {
		return errors.New("config is required")
	}
	deviceID = strings.TrimSpace(deviceID)
	name = strings.TrimSpace(name)
	host = strings.TrimSpace(host)
	room = strings.TrimSpace(room)
	if deviceID == "" {
		return errors.New("device id is required")
	}
	if name == "" {
		return errors.New("device name is required")
	}
	if _, _, err := ResolveConfig(cfg, deviceID); err == nil {
		return fmt.Errorf("dukaone device %q is already configured", deviceID)
	}
	cfg.DukaOne.Devices = append(cfg.DukaOne.Devices, config.DukaOneDeviceConfig{
		DeviceID: deviceID,
		Name:     name,
		Host:     host,
		Room:     room,
	})
	sort.Slice(cfg.DukaOne.Devices, func(i, j int) bool {
		return strings.ToLower(cfg.DukaOne.Devices[i].Name) < strings.ToLower(cfg.DukaOne.Devices[j].Name)
	})
	return nil
}

func DefaultName(deviceID string) string {
	deviceID = strings.TrimSpace(deviceID)
	if len(deviceID) > 6 {
		return "DukaOne " + deviceID[len(deviceID)-6:]
	}
	if deviceID == "" {
		return "DukaOne"
	}
	return "DukaOne " + deviceID
}

func HasConfig(cfg *config.Config, query string) bool {
	if cfg == nil {
		return false
	}
	_, _, err := ResolveConfig(cfg, query)
	return err == nil
}

func Rename(cfg *config.Config, query, newName string) error {
	device, index, err := ResolveConfig(cfg, query)
	if err != nil {
		return err
	}
	device.Name = strings.TrimSpace(newName)
	if device.Name == "" {
		return errors.New("new name is required")
	}
	cfg.DukaOne.Devices[index] = device
	return nil
}

func SetRoom(cfg *config.Config, query, room string) error {
	device, index, err := ResolveConfig(cfg, query)
	if err != nil {
		return err
	}
	device.Room = strings.TrimSpace(room)
	cfg.DukaOne.Devices[index] = device
	return nil
}

func Delete(cfg *config.Config, query string) error {
	_, index, err := ResolveConfig(cfg, query)
	if err != nil {
		return err
	}
	cfg.DukaOne.Devices = append(cfg.DukaOne.Devices[:index], cfg.DukaOne.Devices[index+1:]...)
	return nil
}

func ResolveConfig(cfg *config.Config, query string) (config.DukaOneDeviceConfig, int, error) {
	if cfg == nil {
		return config.DukaOneDeviceConfig{}, -1, errors.New("config is required")
	}
	query = strings.TrimSpace(query)
	rawID := RawID(query)
	lower := strings.ToLower(query)
	for i, device := range cfg.DukaOne.Devices {
		switch {
		case device.DeviceID == rawID:
			return device, i, nil
		case QualifiedID(device.DeviceID) == query:
			return device, i, nil
		case strings.ToLower(strings.TrimSpace(device.Name)) == lower:
			return device, i, nil
		}
	}
	return config.DukaOneDeviceConfig{}, -1, fmt.Errorf("no DukaOne device matched %q", query)
}

func normalize(device config.DukaOneDeviceConfig, state bridgeResponse) map[string]any {
	attrs := map[string]any{}
	if state.IsOn != nil {
		attrs["isOn"] = *state.IsOn
	}
	if state.Mode != "" {
		attrs["mode"] = state.Mode
		attrs["ventilationMode"] = state.Mode
	}
	if state.Speed != "" {
		attrs["preset"] = state.Speed
		attrs["speed"] = state.Speed
	}
	if state.ManualSpeed != nil {
		attrs["manualSpeed"] = *state.ManualSpeed
	}
	if state.Humidity != nil {
		attrs["humidity"] = *state.Humidity
	}
	if state.FilterAlarm != nil {
		attrs["filterAlarm"] = *state.FilterAlarm
	}
	if state.FilterTimer != nil {
		attrs["filterTimer"] = *state.FilterTimer
	}
	if state.FanRPM != nil {
		attrs["fanRPM"] = *state.FanRPM
	}
	if state.FirmwareVersion != "" {
		attrs["firmwareVersion"] = state.FirmwareVersion
	}
	if state.FirmwareDate != "" {
		attrs["firmwareDate"] = state.FirmwareDate
	}
	if state.UnitType != nil {
		attrs["unitType"] = *state.UnitType
	}
	if state.IPAddress != "" {
		attrs["ipAddress"] = state.IPAddress
	}
	item := map[string]any{
		"id":         QualifiedID(device.DeviceID),
		"name":       device.Name,
		"type":       DeviceType,
		"deviceType": DeviceType,
		"category":   DeviceType,
		"source":     "dukaone",
		"attributes": attrs,
		"room": map[string]any{
			"name": device.Room,
		},
		"dukaone": map[string]any{
			"deviceId":       device.DeviceID,
			"configuredHost": firstNonEmpty(device.Host, state.ConfiguredHost),
			"ipAddress":      state.IPAddress,
		},
	}
	return item
}

func runBridge(ctx context.Context, cfg *config.Config, action string, device config.DukaOneDeviceConfig, attrs map[string]any) (bridgeResponse, error) {
	args := []string{
		action,
		"--device-id", device.DeviceID,
		"--ip-address", firstNonEmpty(device.Host, "<broadcast>"),
		"--password", firstNonEmpty(cfg.DukaOne.Password, "1111"),
	}
	if attrs != nil {
		data, err := json.Marshal(attrs)
		if err != nil {
			return bridgeResponse{}, fmt.Errorf("marshal attrs: %w", err)
		}
		args = append(args, "--attrs-json", string(data))
	}
	output, err := runBridgeRaw(ctx, cfg, args...)
	if err != nil {
		return bridgeResponse{}, err
	}
	var response bridgeResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return bridgeResponse{}, fmt.Errorf("parse dukaone bridge response: %w", err)
	}
	return response, nil
}

func runBridgeRaw(ctx context.Context, cfg *config.Config, args ...string) ([]byte, error) {
	scriptPath, err := ensureBridgeScript()
	if err != nil {
		return nil, err
	}
	cmdArgs := append([]string{scriptPath}, args...)
	cmd := exec.CommandContext(ctx, pythonBin(cfg), cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		if strings.Contains(message, "No module named 'dukaonesdk'") {
			return nil, errors.New("dukaonesdk is not installed for the configured Python runtime; run `python3 -m pip install dukaonesdk==1.0.5` or set `dukaone.python` to a Python that already has it")
		}
		return nil, fmt.Errorf("dukaone bridge failed: %s", message)
	}
	return output, nil
}

func ensureBridgeScript() (string, error) {
	dir, err := config.AppDir()
	if err != nil {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, "dukaone_bridge.py")
	current, err := os.ReadFile(path)
	if err == nil && string(current) == bridgeSource {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create DukaOne bridge dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(bridgeSource), 0o700); err != nil {
		return "", fmt.Errorf("write DukaOne bridge: %w", err)
	}
	return path, nil
}

func pythonBin(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.DukaOne.Python) != "" {
		return strings.TrimSpace(cfg.DukaOne.Python)
	}
	return defaultPythonBin
}

func useNative(cfg *config.Config) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.DukaOne.Backend), "native")
}

func stateToBridgeResponse(state native.State) bridgeResponse {
	out := bridgeResponse{
		DeviceID:        state.DeviceID,
		IPAddress:       state.IPAddress,
		FirmwareVersion: state.FirmwareVersion,
		FirmwareDate:    state.FirmwareDate,
		UnitType:        state.UnitType,
		ManualSpeed:     state.ManualSpeed,
		IsOn:            state.IsOn,
		Humidity:        state.Humidity,
		FilterAlarm:     state.FilterAlarm,
		FilterTimer:     state.FilterTimer,
		FanRPM:          state.FanRPM,
	}
	if state.Speed != nil {
		value := state.Speed.String()
		out.Speed = value
	}
	if state.Mode != nil {
		value := state.Mode.String()
		out.Mode = value
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func nameOf(item map[string]any) string {
	if value, ok := item["name"].(string); ok {
		return value
	}
	return ""
}
