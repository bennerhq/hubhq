package native

import "testing"

func TestDecodeResponseStatus(t *testing.T) {
	packet := buildPacket("ABC12345", "1111", funcResponse, []byte{
		paramOnOff, 0x01,
		paramVentilationMode, byte(ModeInOut),
		paramSpeed, byte(SpeedMedium),
		paramManualSpeed, 120,
		paramFan1RPM, 0xD2, 0x04,
		paramFilterAlarm, 0x01,
		paramFilterTimer, 30, 2, 1,
		paramCurrentHumidity, 55,
	})
	state, err := decodeResponse(packet)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if state.DeviceID != "ABC12345" {
		t.Fatalf("unexpected device id: %q", state.DeviceID)
	}
	if state.Mode == nil || *state.Mode != ModeInOut {
		t.Fatalf("unexpected mode: %+v", state.Mode)
	}
	if state.Speed == nil || *state.Speed != SpeedMedium {
		t.Fatalf("unexpected speed: %+v", state.Speed)
	}
	if state.ManualSpeed == nil || *state.ManualSpeed != 120 {
		t.Fatalf("unexpected manual speed: %+v", state.ManualSpeed)
	}
	if state.Humidity == nil || *state.Humidity != 55 {
		t.Fatalf("unexpected humidity: %+v", state.Humidity)
	}
	if state.FilterAlarm == nil || !*state.FilterAlarm {
		t.Fatalf("unexpected filter alarm: %+v", state.FilterAlarm)
	}
	if state.FilterTimer == nil || *state.FilterTimer != 1590 {
		t.Fatalf("unexpected filter timer: %+v", state.FilterTimer)
	}
	if state.FanRPM == nil || *state.FanRPM != 1234 {
		t.Fatalf("unexpected fan rpm: %+v", state.FanRPM)
	}
}

func TestDecodeResponseFirmwareAndSearch(t *testing.T) {
	packet := buildPacket("ABC12345", "1111", funcResponse, []byte{
		paramReadFirmware, 1, 5, 9, 5, 0xE9, 0x07,
		paramUnitType, 0x02, 0x00,
		0xFE, 16, paramSearch, 'A', 'B', 'C', '1', '2', '3', '4', '5', 0, 0, 0, 0, 0, 0, 0, 0,
	})
	state, err := decodeResponse(packet)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if state.FirmwareVersion != "1.5" {
		t.Fatalf("unexpected firmware version: %q", state.FirmwareVersion)
	}
	if state.FirmwareDate != "9-5-2025" {
		t.Fatalf("unexpected firmware date: %q", state.FirmwareDate)
	}
	if state.UnitType == nil || *state.UnitType != 2 {
		t.Fatalf("unexpected unit type: %+v", state.UnitType)
	}
	if state.SearchDeviceID != "ABC12345" {
		t.Fatalf("unexpected search device id: %q", state.SearchDeviceID)
	}
}

func TestDecodeResponseForOffDeviceForcesOffSpeed(t *testing.T) {
	packet := buildPacket("ABC12345", "1111", funcResponse, []byte{
		paramOnOff, 0x00,
		paramSpeed, byte(SpeedHigh),
	})
	state, err := decodeResponse(packet)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if state.IsOn == nil || *state.IsOn {
		t.Fatalf("unexpected on state: %+v", state.IsOn)
	}
	if state.Speed == nil || *state.Speed != SpeedOff {
		t.Fatalf("unexpected speed: %+v", state.Speed)
	}
}
