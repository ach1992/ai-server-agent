package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// RuntimeSettings are user-operational budgets, not absolute content-size or
// security protocol limits. Values are persisted in config.json and become
// effective on the next authenticated Agent/Executor service start.
type RuntimeSettings struct {
	HTTPReadHeaderTimeoutSeconds int `json:"http_read_header_timeout_seconds"`
	HTTPIdleTimeoutSeconds       int `json:"http_idle_timeout_seconds"`
	CommandTimeoutSeconds        int `json:"command_timeout_seconds"`
	WorkspaceFileTimeoutSeconds  int `json:"workspace_file_timeout_seconds"`
	TextFallbackBytes            int `json:"text_fallback_bytes"`
}

type RuntimeSettingSpec struct {
	Key         string `json:"key"`
	Default     int    `json:"default"`
	Minimum     int    `json:"minimum"`
	Maximum     int    `json:"maximum"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
}

var runtimeSettingSpecs = []RuntimeSettingSpec{
	{Key: "http_read_header_timeout_seconds", Default: 10, Minimum: 2, Maximum: 120, Unit: "seconds", Description: "Time allowed for an HTTP client to send request headers; not a long-lived session lifetime"},
	{Key: "http_idle_timeout_seconds", Default: 90, Minimum: 10, Maximum: 3600, Unit: "seconds", Description: "Idle HTTP keep-alive timeout; does not affect MCP credential or terminal session expiry"},
	{Key: "command_timeout_seconds", Default: 300, Minimum: 10, Maximum: 1800, Unit: "seconds", Description: "Maximum synchronous worker/root command duration; use start_job for longer work"},
	{Key: "workspace_file_timeout_seconds", Default: 30, Minimum: 5, Maximum: 300, Unit: "seconds", Description: "Per-call worker workspace file helper timeout; large files use version-pinned windows"},
	{Key: "text_fallback_bytes", Default: 32768, Minimum: 4096, Maximum: 262144, Unit: "bytes", Description: "Maximum serialized text-only MCP fallback; structuredContent and model context have separate budgets"},
}

func RuntimeSettingSpecs() []RuntimeSettingSpec {
	out := make([]RuntimeSettingSpec, len(runtimeSettingSpecs))
	copy(out, runtimeSettingSpecs)
	return out
}

func DefaultRuntimeSettings() RuntimeSettings {
	return RuntimeSettings{
		HTTPReadHeaderTimeoutSeconds: 10,
		HTTPIdleTimeoutSeconds:       90,
		CommandTimeoutSeconds:        300,
		WorkspaceFileTimeoutSeconds:  30,
		TextFallbackBytes:            32768,
	}
}

func (r RuntimeSettings) Validate() error {
	values := map[string]int{
		"http_read_header_timeout_seconds": r.HTTPReadHeaderTimeoutSeconds,
		"http_idle_timeout_seconds":        r.HTTPIdleTimeoutSeconds,
		"command_timeout_seconds":          r.CommandTimeoutSeconds,
		"workspace_file_timeout_seconds":   r.WorkspaceFileTimeoutSeconds,
		"text_fallback_bytes":              r.TextFallbackBytes,
	}
	for _, spec := range runtimeSettingSpecs {
		got := values[spec.Key]
		if got < spec.Minimum || got > spec.Maximum {
			return fmt.Errorf("runtime.%s=%d outside supported range %d..%d %s", spec.Key, got, spec.Minimum, spec.Maximum, spec.Unit)
		}
	}
	return nil
}

func (r *RuntimeSettings) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("runtime settings cannot be null")
	}
	// Keep DefaultRuntimeSettings values for omitted fields. Reject typos:
	// silently ignored "timeout" settings would falsely appear active.
	type alias RuntimeSettings
	value := alias(*r)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("parse runtime settings: %w", err)
	}
	var extra interface{}
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected runtime settings content: %v", err)
	}
	*r = RuntimeSettings(value)
	return nil
}
