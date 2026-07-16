package main

import "testing"

// TestValidateLogFormat asserts only text and json are accepted, and any
// other value is rejected.
func TestValidateLogFormat(t *testing.T) {
	valid := []string{"text", "json"}
	for _, f := range valid {
		cfg := &Config{LogFormat: f}
		if err := cfg.validateLogFormat(); err != nil {
			t.Fatalf("want %q accepted, got error: %v", f, err)
		}
	}

	invalid := []string{"", "xml", "TEXT", "Json"}
	for _, f := range invalid {
		cfg := &Config{LogFormat: f}
		if err := cfg.validateLogFormat(); err == nil {
			t.Fatalf("want %q rejected, got nil error", f)
		}
	}
}
