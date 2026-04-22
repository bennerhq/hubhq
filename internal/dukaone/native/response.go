package native

import "fmt"

var parameterSizes = map[byte]int{
	0x01: 1, 0x02: 1, 0x06: 1, 0x07: 1, 0x0B: 3, 0x0F: 1, 0x14: 1, 0x16: 1, 0x19: 1,
	0x24: 2, 0x25: 1, 0x2D: 1, 0x32: 1, 0x44: 1, 0x4A: 2, 0x4B: 2, 0x64: 3, 0x65: 1,
	0x66: 1, 0x6F: 3, 0x70: 4, 0x72: 1, 0x77: 6, 0x7C: 16, 0x7D: 0, 0x7E: 4, 0x80: 1,
	0x83: 1, 0x85: 1, 0x86: 6, 0x87: 1, 0x88: 1, 0x94: 1, 0x95: 0, 0x96: 0, 0x99: 1,
	0x9A: 1, 0x9B: 1, 0x9C: 4, 0x9D: 4, 0x9E: 4, 0xB7: 1, 0xB9: 2,
}

func decodeResponse(data []byte) (*State, error) {
	if len(data) < 6 {
		return nil, fmt.Errorf("packet too short")
	}
	if data[0] != header0 || data[1] != header1 || data[2] != header2 {
		return nil, fmt.Errorf("invalid header")
	}
	if err := validateChecksum(data); err != nil {
		return nil, err
	}
	pos := 3
	deviceID, pos, err := readString(data, pos)
	if err != nil {
		return nil, err
	}
	_, pos, err = readString(data, pos)
	if err != nil {
		return nil, err
	}
	if pos >= len(data)-2 {
		return nil, fmt.Errorf("missing function")
	}
	if data[pos] != funcResponse {
		return nil, fmt.Errorf("unexpected function %d", data[pos])
	}
	pos++
	state := &State{DeviceID: deviceID}
	for pos < len(data)-2 {
		param := data[pos]
		pos++
		size := 1
		if param == 0xFE {
			if pos+1 >= len(data)-2 {
				return nil, fmt.Errorf("invalid extended parameter")
			}
			size = int(data[pos])
			pos++
			param = data[pos]
			pos++
		} else {
			n, ok := parameterSizes[param]
			if !ok {
				return nil, fmt.Errorf("unknown parameter 0x%X", param)
			}
			size = n
		}
		if pos+size > len(data)-2 {
			return nil, fmt.Errorf("parameter 0x%X overruns packet", param)
		}
		value := data[pos : pos+size]
		switch param {
		case paramOnOff:
			v := value[0] != 0
			state.IsOn = &v
		case paramSpeed:
			v := Speed(value[0])
			state.Speed = &v
		case paramManualSpeed:
			v := int(value[0])
			state.ManualSpeed = &v
		case paramFan1RPM:
			v := int(value[0]) | int(value[1])<<8
			state.FanRPM = &v
		case paramCurrentHumidity:
			v := int(value[0])
			state.Humidity = &v
		case paramVentilationMode:
			v := Mode(value[0])
			state.Mode = &v
		case paramReadFirmware:
			state.FirmwareVersion = fmt.Sprintf("%d.%d", value[0], value[1])
			year := int(value[4]) | int(value[5])<<8
			state.FirmwareDate = fmt.Sprintf("%d-%d-%d", value[2], value[3], year)
		case paramUnitType:
			v := int(value[0])
			state.UnitType = &v
		case paramFilterAlarm:
			v := value[0] != 0
			state.FilterAlarm = &v
		case paramFilterTimer:
			v := int(value[0]) + (int(value[2])*24+int(value[1]))*60
			state.FilterTimer = &v
		case paramSearch:
			state.SearchDeviceID = trimNullASCII(value)
		}
		pos += size
	}
	if state.IsOn != nil && !*state.IsOn {
		v := SpeedOff
		state.Speed = &v
	}
	return state, nil
}

func readString(data []byte, pos int) (string, int, error) {
	if pos >= len(data)-2 {
		return "", pos, fmt.Errorf("short string length")
	}
	size := int(data[pos])
	pos++
	if pos+size > len(data)-2 {
		return "", pos, fmt.Errorf("short string payload")
	}
	return string(data[pos : pos+size]), pos + size, nil
}

func trimNullASCII(value []byte) string {
	end := len(value)
	for end > 0 && value[end-1] == 0 {
		end--
	}
	return string(value[:end])
}
