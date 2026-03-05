package registry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TenantStatus represents the lifecycle state of a tenant
type TenantStatus string

const (
	StatusProvisioning TenantStatus = "provisioning"
	StatusRunning      TenantStatus = "running"
	StatusIdle         TenantStatus = "idle"
	StatusTerminated   TenantStatus = "terminated"
)

// TenantRecord is the DynamoDB schema for a tenant
type TenantRecord struct {
	TenantID      string       `dynamodbav:"tenant_id"`
	Status        TenantStatus `dynamodbav:"status"`
	PodName       string       `dynamodbav:"pod_name,omitempty"`
	PodIP         string       `dynamodbav:"pod_ip,omitempty"`
	Namespace     string       `dynamodbav:"namespace"`
	S3Prefix      string       `dynamodbav:"s3_prefix"`
	BotToken      string       `dynamodbav:"bot_token,omitempty"`
	HooksToken    string       `dynamodbav:"hooks_token,omitempty"`    // OpenClaw Hooks API auth secret
	WebhookSecret string       `dynamodbav:"webhook_secret,omitempty"` // Telegram webhook validation secret
	CreatedAt     time.Time    `dynamodbav:"created_at"`
	LastActiveAt  time.Time    `dynamodbav:"last_active_at"`
	IdleTimeoutS  int64        `dynamodbav:"idle_timeout_s"`
}

// Client is the interface for tenant registry operations
type Client interface {
	GetTenant(ctx context.Context, tenantID string) (*TenantRecord, error)
	CreateTenant(ctx context.Context, record *TenantRecord) error
	UpdateStatus(ctx context.Context, tenantID string, status TenantStatus, podName, podIP string) error
	UpdateActivity(ctx context.Context, tenantID string) error
	UpdateBotToken(ctx context.Context, tenantID, botToken string) error
	UpdateHooksToken(ctx context.Context, tenantID, hooksToken string) error
	UpdateWebhookSecret(ctx context.Context, tenantID, secret string) error
	UpdateIdleTimeout(ctx context.Context, tenantID string, timeoutS int64) error
	ListAll(ctx context.Context) ([]*TenantRecord, error)
	ListByStatus(ctx context.Context, status TenantStatus) ([]*TenantRecord, error)
	ListIdleTenants(ctx context.Context, olderThan time.Duration) ([]*TenantRecord, error)
	DeleteTenant(ctx context.Context, tenantID string) error
}

// GenerateHooksToken generates a cryptographically random 32-byte hex token
// for use as the OpenClaw Hooks API auth secret or Telegram webhook secret.
func GenerateHooksToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateSecret is an alias for GenerateHooksToken.
var GenerateSecret = GenerateHooksToken

// DynamoClient implements Client using AWS DynamoDB.
//
// NOTE (infra prerequisite): This client requires a Global Secondary Index (GSI)
// on the DynamoDB table for efficient status-based queries. Create the GSI:
//
//	GSI Name:      status-index  (configurable via statusIndexName)
//	Partition Key: status (S)
//	Sort Key:      last_active_at (S, ISO 8601 format)
//	Projection:    ALL
//
// Without this GSI, ListByStatus and ListIdleTenants will fail at runtime.
type DynamoClient struct {
	db              *dynamodb.Client
	tableName       string
	statusIndexName string // GSI name, default "status-index"
}

// New creates a new DynamoDB-backed registry client.
// statusIndexName is optional; if empty, defaults to "status-index".
func New(db *dynamodb.Client, tableName string, statusIndexName ...string) *DynamoClient {
	gsi := "status-index"
	if len(statusIndexName) > 0 && statusIndexName[0] != "" {
		gsi = statusIndexName[0]
	}
	return &DynamoClient{db: db, tableName: tableName, statusIndexName: gsi}
}

// GetTenant fetches a tenant record by ID
func (c *DynamoClient) GetTenant(ctx context.Context, tenantID string) (*TenantRecord, error) {
	out, err := c.db.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("dynamodb GetItem: %w", err)
	}
	if out.Item == nil {
		return nil, nil
	}
	var rec TenantRecord
	if err := attributevalue.UnmarshalMap(out.Item, &rec); err != nil {
		return nil, fmt.Errorf("unmarshal tenant: %w", err)
	}
	return &rec, nil
}

// CreateTenant creates a new tenant record (fails if already exists)
func (c *DynamoClient) CreateTenant(ctx context.Context, record *TenantRecord) error {
	item, err := attributevalue.MarshalMap(record)
	if err != nil {
		return fmt.Errorf("marshal tenant: %w", err)
	}
	_, err = c.db.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(c.tableName),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(tenant_id)"),
	})
	if err != nil {
		return fmt.Errorf("dynamodb PutItem: %w", err)
	}
	return nil
}

// UpdateStatus updates tenant status, pod name, and pod IP atomically
func (c *DynamoClient) UpdateStatus(ctx context.Context, tenantID string, status TenantStatus, podName, podIP string) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
		UpdateExpression: aws.String("SET #s = :s, pod_name = :pn, pod_ip = :pi, last_active_at = :la"),
		ExpressionAttributeNames: map[string]string{
			"#s": "status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":s":  &types.AttributeValueMemberS{Value: string(status)},
			":pn": &types.AttributeValueMemberS{Value: podName},
			":pi": &types.AttributeValueMemberS{Value: podIP},
			":la": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("dynamodb UpdateItem: %w", err)
	}
	return nil
}

// UpdateActivity updates the last_active_at timestamp
func (c *DynamoClient) UpdateActivity(ctx context.Context, tenantID string) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
		UpdateExpression: aws.String("SET last_active_at = :la"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":la": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	return err
}

// UpdateBotToken updates the bot_token for a tenant
func (c *DynamoClient) UpdateBotToken(ctx context.Context, tenantID, botToken string) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
		UpdateExpression: aws.String("SET bot_token = :bt"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":bt": &types.AttributeValueMemberS{Value: botToken},
		},
		ConditionExpression: aws.String("attribute_exists(tenant_id)"),
	})
	return err
}

// UpdateHooksToken updates the hooks_token for a tenant
func (c *DynamoClient) UpdateHooksToken(ctx context.Context, tenantID, hooksToken string) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
		UpdateExpression: aws.String("SET hooks_token = :ht"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ht": &types.AttributeValueMemberS{Value: hooksToken},
		},
		ConditionExpression: aws.String("attribute_exists(tenant_id)"),
	})
	return err
}

// UpdateWebhookSecret updates the webhook_secret for a tenant
func (c *DynamoClient) UpdateWebhookSecret(ctx context.Context, tenantID, secret string) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
		UpdateExpression: aws.String("SET webhook_secret = :ws"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":ws": &types.AttributeValueMemberS{Value: secret},
		},
		ConditionExpression: aws.String("attribute_exists(tenant_id)"),
	})
	return err
}

// UpdateIdleTimeout updates the idle_timeout_s for a tenant
func (c *DynamoClient) UpdateIdleTimeout(ctx context.Context, tenantID string, timeoutS int64) error {
	_, err := c.db.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
		UpdateExpression: aws.String("SET idle_timeout_s = :t"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":t": &types.AttributeValueMemberN{Value: fmt.Sprintf("%d", timeoutS)},
		},
		ConditionExpression: aws.String("attribute_exists(tenant_id)"),
	})
	return err
}

// ListAll returns all tenant records (excluding internal warm-pool metadata).
// Handles pagination via LastEvaluatedKey for large tables (10K+ records).
func (c *DynamoClient) ListAll(ctx context.Context) ([]*TenantRecord, error) {
	var records []*TenantRecord
	var exclusiveStartKey map[string]types.AttributeValue

	for {
		input := &dynamodb.ScanInput{
			TableName:        aws.String(c.tableName),
			FilterExpression: aws.String("attribute_exists(#s) AND tenant_id <> :meta"),
			ExpressionAttributeNames: map[string]string{
				"#s": "status",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":meta": &types.AttributeValueMemberS{Value: "warm-pool-meta"},
			},
		}
		if exclusiveStartKey != nil {
			input.ExclusiveStartKey = exclusiveStartKey
		}

		out, err := c.db.Scan(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("dynamodb Scan: %w", err)
		}

		for _, item := range out.Items {
			var rec TenantRecord
			if err := attributevalue.UnmarshalMap(item, &rec); err != nil {
				continue
			}
			records = append(records, &rec)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		exclusiveStartKey = out.LastEvaluatedKey
	}

	return records, nil
}

// ListByStatus returns all tenants with the given status.
// Uses Query on the status-index GSI (partition key: status) for efficient lookups.
func (c *DynamoClient) ListByStatus(ctx context.Context, status TenantStatus) ([]*TenantRecord, error) {
	var records []*TenantRecord
	var exclusiveStartKey map[string]types.AttributeValue

	for {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(c.tableName),
			IndexName:              aws.String(c.statusIndexName),
			KeyConditionExpression: aws.String("#s = :status"),
			ExpressionAttributeNames: map[string]string{
				"#s": "status",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":status": &types.AttributeValueMemberS{Value: string(status)},
			},
		}
		if exclusiveStartKey != nil {
			input.ExclusiveStartKey = exclusiveStartKey
		}

		out, err := c.db.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("dynamodb Query (status-index): %w", err)
		}

		for _, item := range out.Items {
			var rec TenantRecord
			if err := attributevalue.UnmarshalMap(item, &rec); err != nil {
				continue
			}
			records = append(records, &rec)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		exclusiveStartKey = out.LastEvaluatedKey
	}

	return records, nil
}

// ListIdleTenants returns running tenants whose last_active_at is older than olderThan.
// Uses Query on the status-index GSI with partition key = "running" and
// sort key condition last_active_at < cutoff for efficient lookups.
func (c *DynamoClient) ListIdleTenants(ctx context.Context, olderThan time.Duration) ([]*TenantRecord, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)

	var records []*TenantRecord
	var exclusiveStartKey map[string]types.AttributeValue

	for {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(c.tableName),
			IndexName:              aws.String(c.statusIndexName),
			KeyConditionExpression: aws.String("#s = :running AND last_active_at < :cutoff"),
			ExpressionAttributeNames: map[string]string{
				"#s": "status",
			},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":running": &types.AttributeValueMemberS{Value: string(StatusRunning)},
				":cutoff":  &types.AttributeValueMemberS{Value: cutoff},
			},
		}
		if exclusiveStartKey != nil {
			input.ExclusiveStartKey = exclusiveStartKey
		}

		out, err := c.db.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("dynamodb Query (status-index, idle): %w", err)
		}

		for _, item := range out.Items {
			var rec TenantRecord
			if err := attributevalue.UnmarshalMap(item, &rec); err != nil {
				continue
			}
			records = append(records, &rec)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		exclusiveStartKey = out.LastEvaluatedKey
	}

	return records, nil
}

// DeleteTenant removes a tenant record
func (c *DynamoClient) DeleteTenant(ctx context.Context, tenantID string) error {
	_, err := c.db.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"tenant_id": &types.AttributeValueMemberS{Value: tenantID},
		},
	})
	return err
}
