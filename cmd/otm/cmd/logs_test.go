package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildLogsArgs_Defaults(t *testing.T) {
	args := buildLogsArgs("tenants", "", "shawn", "openclaw", 0, true, false)
	expected := []string{"logs", "-n", "tenants", "shawn", "-c", "openclaw", "-f"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_CustomContainer(t *testing.T) {
	args := buildLogsArgs("tenants", "", "shawn", "s3-sync", 0, true, false)
	expected := []string{"logs", "-n", "tenants", "shawn", "-c", "s3-sync", "-f"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_WithTail(t *testing.T) {
	args := buildLogsArgs("tenants", "", "shawn", "openclaw", 100, true, false)
	expected := []string{"logs", "-n", "tenants", "shawn", "-c", "openclaw", "-f", "--tail", "100"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_WithPrevious(t *testing.T) {
	args := buildLogsArgs("tenants", "", "shawn", "openclaw", 0, true, true)
	expected := []string{"logs", "-n", "tenants", "shawn", "-c", "openclaw", "-f", "--previous"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_NoFollow(t *testing.T) {
	args := buildLogsArgs("tenants", "", "shawn", "openclaw", 0, false, false)
	expected := []string{"logs", "-n", "tenants", "shawn", "-c", "openclaw"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_CustomNamespaceAndContext(t *testing.T) {
	args := buildLogsArgs("prod", "my-cluster", "shawn", "openclaw", 0, true, false)
	expected := []string{"logs", "-n", "prod", "--context", "my-cluster", "shawn", "-c", "openclaw", "-f"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_EmptyNamespace(t *testing.T) {
	args := buildLogsArgs("", "", "shawn", "openclaw", 0, false, false)
	expected := []string{"logs", "shawn", "-c", "openclaw"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_EmptyContainer(t *testing.T) {
	args := buildLogsArgs("tenants", "", "shawn", "", 0, false, false)
	expected := []string{"logs", "-n", "tenants", "shawn"}
	assert.Equal(t, expected, args)
}

func TestBuildLogsArgs_AllOptions(t *testing.T) {
	args := buildLogsArgs("prod", "eks-prod", "alice", "s3-restore", 50, true, true)
	expected := []string{"logs", "-n", "prod", "--context", "eks-prod", "alice", "-c", "s3-restore", "-f", "--previous", "--tail", "50"}
	assert.Equal(t, expected, args)
}

func TestPodNameFromTenantID(t *testing.T) {
	tests := []struct {
		tenantID string
		expected string
	}{
		{"shawn", "shawn"},
		{"alice", "alice"},
		{"test-tenant", "test-tenant"},
	}
	for _, tc := range tests {
		t.Run(tc.tenantID, func(t *testing.T) {
			assert.Equal(t, tc.expected, tc.tenantID)
		})
	}
}
