package native

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Port int
}

func New() *Client {
	return &Client{Port: udpPort}
}

func (c *Client) Discover(ctx context.Context, broadcast string, timeout time.Duration) ([]State, error) {
	conn, err := c.listen()
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	target := firstNonEmpty(strings.TrimSpace(broadcast), "255.255.255.255")
	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", target, c.Port))
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(encodeSearch(), addr); err != nil {
		return nil, err
	}

	found := map[string]State{}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 1024)
	for {
		if err := conn.SetReadDeadline(nextDeadline(ctx, deadline)); err != nil {
			return nil, err
		}
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) && time.Now().After(deadline) {
				break
			}
			if isTimeout(err) {
				continue
			}
			return nil, err
		}
		resp, err := decodeResponse(buf[:n])
		if err != nil || strings.TrimSpace(resp.SearchDeviceID) == "" {
			continue
		}
		item := found[resp.SearchDeviceID]
		item.DeviceID = resp.SearchDeviceID
		item.IPAddress = remote.IP.String()
		found[resp.SearchDeviceID] = item
	}
	out := make([]State, 0, len(found))
	for _, item := range found {
		state, err := c.ReadState(ctx, item.DeviceID, "1111", item.IPAddress, 2*time.Second)
		if err != nil {
			out = append(out, item)
			continue
		}
		out = append(out, state)
	}
	return out, nil
}

func (c *Client) ReadState(ctx context.Context, deviceID, password, host string, timeout time.Duration) (State, error) {
	conn, err := c.listen()
	if err != nil {
		return State{}, err
	}
	defer conn.Close()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", firstNonEmpty(host, "255.255.255.255"), c.Port))
	if err != nil {
		return State{}, err
	}
	firmwarePacket := encodeFirmware(deviceID, password)
	statusPacket := encodeStatus(deviceID, password)
	if _, err := conn.WriteToUDP(firmwarePacket, addr); err != nil {
		return State{}, err
	}
	time.Sleep(150 * time.Millisecond)
	if _, err := conn.WriteToUDP(statusPacket, addr); err != nil {
		return State{}, err
	}

	state := State{DeviceID: deviceID}
	deadline := time.Now().Add(timeout)
	nextStatusRetry := time.Now().Add(400 * time.Millisecond)
	buf := make([]byte, 1024)
	for {
		if !hasLiveStatus(state) && time.Now().After(nextStatusRetry) {
			if _, err := conn.WriteToUDP(statusPacket, addr); err != nil {
				return State{}, err
			}
			nextStatusRetry = time.Now().Add(400 * time.Millisecond)
		}
		if err := conn.SetReadDeadline(nextDeadline(ctx, deadline)); err != nil {
			return State{}, err
		}
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if isTimeout(err) && time.Now().After(deadline) {
				break
			}
			if isTimeout(err) {
				continue
			}
			return State{}, err
		}
		resp, err := decodeResponse(buf[:n])
		if err != nil || resp.DeviceID != deviceID {
			continue
		}
		resp.IPAddress = remote.IP.String()
		mergeState(&state, *resp)
		if hasLiveStatus(state) {
			break
		}
	}
	if !hasLiveStatus(state) && state.FirmwareVersion == "" {
		return State{}, fmt.Errorf("no response from device %s", deviceID)
	}
	return state, nil
}

func (c *Client) SetAttributes(ctx context.Context, deviceID, password, host string, attrs map[string]any, timeout time.Duration) (State, error) {
	conn, err := c.listen()
	if err != nil {
		return State{}, err
	}
	defer conn.Close()

	addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", firstNonEmpty(host, "255.255.255.255"), c.Port))
	if err != nil {
		return State{}, err
	}

	packets, err := buildWritePackets(deviceID, password, attrs)
	if err != nil {
		return State{}, err
	}
	for _, packet := range packets {
		if _, err := conn.WriteToUDP(packet, addr); err != nil {
			return State{}, err
		}
		time.Sleep(200 * time.Millisecond)
	}

	return c.ReadState(ctx, deviceID, password, host, timeout)
}

func (c *Client) listen() (*net.UDPConn, error) {
	return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: c.Port})
}

func mergeState(dst *State, src State) {
	if src.DeviceID != "" {
		dst.DeviceID = src.DeviceID
	}
	if src.IPAddress != "" {
		dst.IPAddress = src.IPAddress
	}
	if src.Speed != nil {
		dst.Speed = src.Speed
	}
	if src.Mode != nil {
		dst.Mode = src.Mode
	}
	if src.ManualSpeed != nil {
		dst.ManualSpeed = src.ManualSpeed
	}
	if src.IsOn != nil {
		dst.IsOn = src.IsOn
	}
	if src.Humidity != nil {
		dst.Humidity = src.Humidity
	}
	if src.FilterAlarm != nil {
		dst.FilterAlarm = src.FilterAlarm
	}
	if src.FilterTimer != nil {
		dst.FilterTimer = src.FilterTimer
	}
	if src.FanRPM != nil {
		dst.FanRPM = src.FanRPM
	}
	if src.FirmwareVersion != "" {
		dst.FirmwareVersion = src.FirmwareVersion
	}
	if src.FirmwareDate != "" {
		dst.FirmwareDate = src.FirmwareDate
	}
	if src.UnitType != nil {
		dst.UnitType = src.UnitType
	}
}

func hasLiveStatus(state State) bool {
	return state.IsOn != nil ||
		state.Speed != nil ||
		state.Mode != nil ||
		state.ManualSpeed != nil ||
		state.Humidity != nil ||
		state.FilterAlarm != nil ||
		state.FilterTimer != nil ||
		state.FanRPM != nil
}

func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return false
}

func nextDeadline(ctx context.Context, deadline time.Time) time.Time {
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func buildWritePackets(deviceID, password string, attrs map[string]any) ([][]byte, error) {
	packets := make([][]byte, 0, len(attrs))

	modeValue, hasMode := attrs["mode"]
	if !hasMode {
		modeValue, hasMode = attrs["ventilationMode"]
	}
	if hasMode {
		mode, err := parseMode(modeValue)
		if err != nil {
			return nil, err
		}
		packets = append(packets, encodeSetMode(deviceID, password, mode))
	}

	presetValue, hasPreset := attrs["preset"]
	if !hasPreset {
		presetValue, hasPreset = attrs["speed"]
	}
	if hasPreset {
		speed, err := parseSpeed(presetValue)
		if err != nil {
			return nil, err
		}
		packets = append(packets, encodeSetSpeed(deviceID, password, speed))
	}

	if manualValue, ok := attrs["manualSpeed"]; ok {
		manual, err := parseInt(manualValue)
		if err != nil {
			return nil, fmt.Errorf("manualSpeed: %w", err)
		}
		if manual < 0 || manual > 255 {
			return nil, fmt.Errorf("manualSpeed must be between 0 and 255")
		}
		packets = append(packets, encodeSetManualSpeed(deviceID, password, manual))
	}

	if resetValue, ok := attrs["resetFilter"]; ok {
		reset, err := parseBool(resetValue)
		if err != nil {
			return nil, fmt.Errorf("resetFilter: %w", err)
		}
		if reset {
			packets = append(packets, encodeResetFilterTimer(deviceID, password))
		}
	}
	if resetValue, ok := attrs["resetFilterAlarm"]; ok {
		reset, err := parseBool(resetValue)
		if err != nil {
			return nil, fmt.Errorf("resetFilterAlarm: %w", err)
		}
		if reset {
			packets = append(packets, encodeResetFilterTimer(deviceID, password))
		}
	}

	if onValue, ok := attrs["isOn"]; ok {
		on, err := parseBool(onValue)
		if err != nil {
			return nil, fmt.Errorf("isOn: %w", err)
		}
		if on {
			packets = append(packets, encodeTurnOn(deviceID, password))
		} else {
			packets = append(packets, encodeTurnOff(deviceID, password))
		}
	}

	if len(packets) == 0 {
		return nil, fmt.Errorf("no supported DukaOne attributes supplied")
	}
	return packets, nil
}

func parseMode(value any) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(value))) {
	case "out", "oneway":
		return ModeOut, nil
	case "inout", "twoway":
		return ModeInOut, nil
	case "in":
		return ModeIn, nil
	default:
		return 0, fmt.Errorf("unsupported mode %q", value)
	}
}

func parseSpeed(value any) (Speed, error) {
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(value))) {
	case "off":
		return SpeedOff, nil
	case "low":
		return SpeedLow, nil
	case "medium":
		return SpeedMedium, nil
	case "high":
		return SpeedHigh, nil
	case "manual":
		return SpeedManual, nil
	default:
		return 0, fmt.Errorf("unsupported preset %q", value)
	}
}

func parseInt(value any) (int, error) {
	switch current := value.(type) {
	case int:
		return current, nil
	case int64:
		return int(current), nil
	case float64:
		return int(current), nil
	default:
		return strconv.Atoi(strings.TrimSpace(fmt.Sprint(value)))
	}
}

func parseBool(value any) (bool, error) {
	switch current := value.(type) {
	case bool:
		return current, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(current)) {
		case "true", "1", "yes", "on":
			return true, nil
		case "false", "0", "no", "off":
			return false, nil
		}
	}
	return false, fmt.Errorf("unsupported boolean %v", value)
}
