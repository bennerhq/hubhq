package native

type Mode uint8

const (
	ModeOut   Mode = 0
	ModeInOut Mode = 1
	ModeIn    Mode = 2
)

func (m Mode) String() string {
	switch m {
	case ModeOut:
		return "out"
	case ModeInOut:
		return "inout"
	case ModeIn:
		return "in"
	default:
		return ""
	}
}

type Speed uint8

const (
	SpeedOff    Speed = 0
	SpeedLow    Speed = 1
	SpeedMedium Speed = 2
	SpeedHigh   Speed = 3
	SpeedManual Speed = 255
)

func (s Speed) String() string {
	switch s {
	case SpeedOff:
		return "off"
	case SpeedLow:
		return "low"
	case SpeedMedium:
		return "medium"
	case SpeedHigh:
		return "high"
	case SpeedManual:
		return "manual"
	default:
		return ""
	}
}

type State struct {
	DeviceID        string
	IPAddress       string
	Speed           *Speed
	Mode            *Mode
	ManualSpeed     *int
	IsOn            *bool
	Humidity        *int
	FilterAlarm     *bool
	FilterTimer     *int
	FanRPM          *int
	FirmwareVersion string
	FirmwareDate    string
	UnitType        *int
	SearchDeviceID  string
}
