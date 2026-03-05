package output

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStyler_NoColor(t *testing.T) {
	s := NewStyler(true) // noColor = true
	result := s.Success("test")
	assert.Equal(t, "✓ test", result)
}

func TestStyler_WithColor(t *testing.T) {
	s := NewStyler(false) // noColor = false
	result := s.Success("test")
	assert.Contains(t, result, "✓")
	assert.Contains(t, result, "test")
	// Should contain ANSI codes
	assert.Contains(t, result, "\033[")
}

func TestStyler_Error(t *testing.T) {
	s := NewStyler(true)
	result := s.Error("failed")
	assert.Equal(t, "✗ failed", result)
}

func TestStyler_Info(t *testing.T) {
	s := NewStyler(true)
	result := s.Info("info message")
	assert.Equal(t, "ℹ info message", result)
}

func TestStyler_Warn(t *testing.T) {
	s := NewStyler(true)
	result := s.Warn("warning")
	assert.Equal(t, "⚠ warning", result)
}

func TestFormatJSON(t *testing.T) {
	data := map[string]interface{}{
		"tenant_id": "alice",
		"status":    "running",
	}

	result, err := FormatJSON(data)
	assert.NoError(t, err)
	assert.Contains(t, result, "alice")
	assert.Contains(t, result, "running")
	assert.Contains(t, result, "\n") // Pretty-printed
}

func TestFormatJSON_Error(t *testing.T) {
	// channels cannot be marshaled
	data := make(chan int)

	_, err := FormatJSON(data)
	assert.Error(t, err)
}
