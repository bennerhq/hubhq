package script

type Document struct {
	Language       string   `json:"language"`
	Runtime        string   `json:"runtime,omitempty"`
	EntryPoint     string   `json:"entry_point,omitempty"`
	Source         string   `json:"source"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
}
