package version

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// Major is incremented for breaking releases.
	Major = 0
	// Minor is incremented for backward-compatible feature releases.
	Minor = 1
)

var (
	Patch = "0"
)

type Info struct {
	Version  string
	Date     string
	Time     string
	Location string
}

func String() string {
	base := fmt.Sprintf("v%d.%d.%s", Major, Minor, normalizePatch(Patch))
	return fmt.Sprintf("%s-%s", base, buildSequence())
}

func Current() Info {
	location, modTime := currentBinaryInfo()
	date := ""
	clock := ""
	if !modTime.IsZero() {
		local := modTime.Local()
		date = local.Format("2006-01-02")
		clock = local.Format("15:04:05 MST")
	}
	return Info{
		Version:  String(),
		Date:     date,
		Time:     clock,
		Location: location,
	}
}

func normalizePatch(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "0"
	}
	return value
}

type sequenceState struct {
	BinaryModTime string `json:"binary_mod_time"`
	Sequence      int    `json:"sequence"`
}

func buildSequence() string {
	_, modTime := currentBinaryInfo()
	if modTime.IsZero() {
		return "000"
	}
	modTimeText := modTime.UTC().Format(time.RFC3339Nano)
	state, err := loadSequenceState()
	if err != nil {
		return "000"
	}
	if state.BinaryModTime != modTimeText {
		state.Sequence++
		state.BinaryModTime = modTimeText
		if state.Sequence <= 0 {
			state.Sequence = 1
		}
		if err := saveSequenceState(state); err != nil {
			return fmt.Sprintf("%03d", state.Sequence)
		}
	}
	if state.Sequence <= 0 {
		state.Sequence = 1
	}
	return fmt.Sprintf("%03d", state.Sequence)
}

func currentBinaryInfo() (string, time.Time) {
	exe, err := os.Executable()
	if err != nil {
		return "", time.Time{}
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && strings.TrimSpace(resolved) != "" {
		exe = resolved
	}
	info, err := os.Stat(exe)
	if err != nil {
		return exe, time.Time{}
	}
	return exe, info.ModTime()
}

func loadSequenceState() (sequenceState, error) {
	path, err := statePath()
	if err != nil {
		return sequenceState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if legacyPath, legacyErr := legacyStatePath(); legacyErr == nil {
				legacyData, legacyReadErr := os.ReadFile(legacyPath)
				if legacyReadErr == nil {
					var state sequenceState
					if err := json.Unmarshal(legacyData, &state); err == nil {
						_ = saveSequenceState(state)
						return state, nil
					}
				}
			}
			return sequenceState{}, nil
		}
		return sequenceState{}, err
	}
	var state sequenceState
	if err := json.Unmarshal(data, &state); err != nil {
		return sequenceState{}, err
	}
	return state, nil
}

func saveSequenceState(state sequenceState) error {
	path, err := statePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func statePath() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("resolve version state source path")
	}
	base := filepath.Dir(file)
	if base == "" {
		return "", fmt.Errorf("resolve version state source directory")
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve version state source directory: %w", err)
	}
	return filepath.Join(base, "version-sequence.json"), nil
}

func legacyStatePath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "hubhq", "version-sequence.json"), nil
}

func CurrentSequence() int {
	value := buildSequence()
	seq, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return seq
}
