package cmd

import (
	"bytes"
	stdcontext "context"
	"fmt"
	"testing"

	"github.com/aws-samples/sample-multi-tenancy-openclaw-on-eks/internal/cli/api"
	"github.com/stretchr/testify/assert"
)

func TestConfigGetCommand(t *testing.T) {
	mockClient := &api.MockClient{
		GetConfigFunc: func(ctx stdcontext.Context) (*api.RuntimeConfig, error) {
			return &api.RuntimeConfig{WarmPoolTarget: 10}, nil
		},
	}

	cmd := newConfigGetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	assert.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "10")
}

func TestConfigGetCommand_JSON(t *testing.T) {
	mockClient := &api.MockClient{
		GetConfigFunc: func(ctx stdcontext.Context) (*api.RuntimeConfig, error) {
			return &api.RuntimeConfig{WarmPoolTarget: 7}, nil
		},
	}

	cmd := newConfigGetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)

	// Need to set the global outputFormat
	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	cmd.SetArgs([]string{})
	err := cmd.Execute()
	assert.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, `"warm_pool_target": 7`)
}

func TestConfigGetCommand_Error(t *testing.T) {
	mockClient := &api.MockClient{
		GetConfigFunc: func(ctx stdcontext.Context) (*api.RuntimeConfig, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}

	cmd := newConfigGetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{})

	err := cmd.Execute()
	assert.Error(t, err)
}

func TestConfigSetCommand_WarmPoolTarget(t *testing.T) {
	var receivedCfg *api.RuntimeConfig
	mockClient := &api.MockClient{
		UpdateConfigFunc: func(ctx stdcontext.Context, cfg *api.RuntimeConfig) (*api.RuntimeConfig, error) {
			receivedCfg = cfg
			return cfg, nil
		},
	}

	cmd := newConfigSetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"warm-pool-target", "15"})

	err := cmd.Execute()
	assert.NoError(t, err)
	assert.NotNil(t, receivedCfg)
	assert.Equal(t, 15, receivedCfg.WarmPoolTarget)
}

func TestConfigSetCommand_InvalidValue(t *testing.T) {
	mockClient := &api.MockClient{}

	cmd := newConfigSetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"warm-pool-target", "abc"})

	err := cmd.Execute()
	assert.Error(t, err)
}

func TestConfigSetCommand_NegativeValue(t *testing.T) {
	mockClient := &api.MockClient{}

	cmd := newConfigSetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"warm-pool-target", "-5"})

	err := cmd.Execute()
	assert.Error(t, err)
}

func TestConfigSetCommand_UnknownKey(t *testing.T) {
	mockClient := &api.MockClient{}

	cmd := newConfigSetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"unknown-key", "123"})

	err := cmd.Execute()
	assert.Error(t, err)
}

func TestConfigSetCommand_MissingArgs(t *testing.T) {
	mockClient := &api.MockClient{}

	cmd := newConfigSetCmd(mockClient)
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"warm-pool-target"})

	err := cmd.Execute()
	assert.Error(t, err)
}
