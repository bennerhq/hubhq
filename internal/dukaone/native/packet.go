package native

import "fmt"

const (
	udpPort = 4000
	header0 = 0xFD
	header1 = 0xFD
	header2 = 0x02
)

const (
	funcRead      = 1
	funcWrite     = 2
	funcWriteRead = 3
	funcResponse  = 6
)

const (
	paramOnOff            = 0x01
	paramSpeed            = 0x02
	paramCurrentHumidity  = 0x25
	paramManualSpeed      = 0x44
	paramFan1RPM          = 0x4A
	paramFilterTimer      = 0x64
	paramResetFilterTimer = 0x65
	paramSearch           = 0x7C
	paramReadFirmware     = 0x86
	paramFilterAlarm      = 0x88
	paramVentilationMode  = 0xB7
	paramUnitType         = 0xB9
)

func encodeSearch() []byte {
	return buildPacket("DEFAULT_DEVICEID", "", funcRead, []byte{paramSearch})
}

func encodeStatus(deviceID, password string) []byte {
	return buildPacket(deviceID, password, funcRead, []byte{
		paramOnOff,
		paramVentilationMode,
		paramSpeed,
		paramManualSpeed,
		paramFan1RPM,
		paramFilterAlarm,
		paramFilterTimer,
		paramCurrentHumidity,
	})
}

func encodeFirmware(deviceID, password string) []byte {
	return buildPacket(deviceID, password, funcRead, []byte{
		paramReadFirmware,
		paramUnitType,
	})
}

func encodeTurnOn(deviceID, password string) []byte {
	return buildPacket(deviceID, password, funcWriteRead, []byte{
		paramOnOff,
		0x01,
	})
}

func encodeTurnOff(deviceID, password string) []byte {
	return buildPacket(deviceID, password, funcWriteRead, []byte{
		paramOnOff,
		0x00,
	})
}

func encodeSetSpeed(deviceID, password string, speed Speed) []byte {
	return buildPacket(deviceID, password, funcWriteRead, []byte{
		paramSpeed,
		byte(speed),
	})
}

func encodeSetManualSpeed(deviceID, password string, manualSpeed int) []byte {
	return buildPacket(deviceID, password, funcWriteRead, []byte{
		paramManualSpeed,
		byte(manualSpeed),
	})
}

func encodeSetMode(deviceID, password string, mode Mode) []byte {
	return buildPacket(deviceID, password, funcWriteRead, []byte{
		paramVentilationMode,
		byte(mode),
	})
}

func encodeResetFilterTimer(deviceID, password string) []byte {
	return buildPacket(deviceID, password, funcWrite, []byte{
		paramResetFilterTimer,
	})
}

func buildPacket(deviceID, password string, fn byte, params []byte) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, header0, header1, header2)
	buf = appendString(buf, deviceID)
	buf = appendString(buf, password)
	buf = append(buf, fn)
	buf = append(buf, params...)
	checksum := checksum(buf)
	buf = append(buf, byte(checksum&0xFF), byte((checksum>>8)&0xFF))
	return buf
}

func appendString(buf []byte, value string) []byte {
	buf = append(buf, byte(len(value)))
	buf = append(buf, []byte(value)...)
	return buf
}

func checksum(data []byte) uint16 {
	var sum uint16
	for i := 2; i < len(data); i++ {
		sum += uint16(data[i])
	}
	return sum
}

func validateChecksum(data []byte) error {
	if len(data) < 5 {
		return fmt.Errorf("packet too short")
	}
	expected := checksum(data[:len(data)-2])
	actual := uint16(data[len(data)-2]) | uint16(data[len(data)-1])<<8
	if expected != actual {
		return fmt.Errorf("checksum mismatch")
	}
	return nil
}
