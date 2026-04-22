package native

import "testing"

func TestEncodeSearch(t *testing.T) {
	packet := encodeSearch()
	if len(packet) < 10 {
		t.Fatalf("search packet too short: %d", len(packet))
	}
	if packet[0] != header0 || packet[1] != header1 || packet[2] != header2 {
		t.Fatalf("unexpected header: %v", packet[:3])
	}
	if got := string(packet[4 : 4+len("DEFAULT_DEVICEID")]); got != "DEFAULT_DEVICEID" {
		t.Fatalf("unexpected device id: %q", got)
	}
	if packet[len(packet)-4] != funcRead {
		t.Fatalf("unexpected function byte: %d", packet[len(packet)-4])
	}
	if packet[len(packet)-3] != paramSearch {
		t.Fatalf("unexpected search parameter: %d", packet[len(packet)-3])
	}
	if err := validateChecksum(packet); err != nil {
		t.Fatalf("checksum validation failed: %v", err)
	}
}

func TestEncodeStatus(t *testing.T) {
	packet := encodeStatus("ABC12345", "1111")
	if packet[0] != header0 || packet[1] != header1 || packet[2] != header2 {
		t.Fatalf("unexpected header: %v", packet[:3])
	}
	if got := packet[3]; got != byte(len("ABC12345")) {
		t.Fatalf("unexpected device id length: %d", got)
	}
	if err := validateChecksum(packet); err != nil {
		t.Fatalf("checksum validation failed: %v", err)
	}
}

func TestEncodeWriteCommands(t *testing.T) {
	cases := []struct {
		name   string
		packet []byte
		fn     byte
		param  byte
		value  byte
	}{
		{name: "turn on", packet: encodeTurnOn("ABC12345", "1111"), fn: funcWriteRead, param: paramOnOff, value: 0x01},
		{name: "turn off", packet: encodeTurnOff("ABC12345", "1111"), fn: funcWriteRead, param: paramOnOff, value: 0x00},
		{name: "speed", packet: encodeSetSpeed("ABC12345", "1111", SpeedMedium), fn: funcWriteRead, param: paramSpeed, value: byte(SpeedMedium)},
		{name: "manual", packet: encodeSetManualSpeed("ABC12345", "1111", 120), fn: funcWriteRead, param: paramManualSpeed, value: 120},
		{name: "mode", packet: encodeSetMode("ABC12345", "1111", ModeInOut), fn: funcWriteRead, param: paramVentilationMode, value: byte(ModeInOut)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateChecksum(tc.packet); err != nil {
				t.Fatalf("checksum validation failed: %v", err)
			}
			if got := tc.packet[len(tc.packet)-5]; got != tc.fn {
				t.Fatalf("unexpected function byte: got %d want %d", got, tc.fn)
			}
			if got := tc.packet[len(tc.packet)-4]; got != tc.param {
				t.Fatalf("unexpected parameter byte: got %d want %d", got, tc.param)
			}
			if got := tc.packet[len(tc.packet)-3]; got != tc.value {
				t.Fatalf("unexpected parameter value: got %d want %d", got, tc.value)
			}
		})
	}
}
