package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVersionCommand(t *testing.T) {
	// Set test version info
	SetVersion("v0.1.0-test", "abc123", "2026-02-23T10:00:00Z")

	cmd := newVersionCmd()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	err := cmd.Execute()
	assert.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "v0.1.0-test")
	assert.Contains(t, output, "abc123")
	assert.Contains(t, output, "2026-02-23T10:00:00Z")
}
