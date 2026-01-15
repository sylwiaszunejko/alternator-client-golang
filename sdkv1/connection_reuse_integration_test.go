//go:build integration
// +build integration

package sdkv1_test

import (
	"context"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"

	helper "github.com/scylladb/alternator-client-golang/sdkv1"
)

// TestHTTPConnectionReuse verifies that the client reuses HTTP connections
// instead of creating new connections for each request.
func TestHTTPConnectionReuse(t *testing.T) {
	t.Run("HTTP", func(t *testing.T) {
		testConnectionReuse(t, "http", httpPort, false)
	})

	t.Run("HTTPS", func(t *testing.T) {
		testConnectionReuse(t, "https", httpsPort, true)
	})
}

func testConnectionReuse(t *testing.T, scheme string, port int, ignoreCertErrors bool) {
	opts := []helper.Option{
		helper.WithScheme(scheme),
		helper.WithPort(port),
		helper.WithNodesListUpdatePeriod(0),
		helper.WithIdleNodesListUpdatePeriod(0),
		helper.WithCredentials("whatever", "secret"),
	}

	if ignoreCertErrors {
		opts = append(opts, helper.WithIgnoreServerCertificateError(true))
	}

	h, err := helper.NewHelper(knownNodes, opts...)
	if err != nil {
		t.Fatalf("failed to create alternator helper: %v", err)
	}
	defer h.Stop()

	ddb, err := h.NewDynamoDB()
	if err != nil {
		t.Fatalf("failed to create DynamoDB client: %v", err)
	}

	tableName := "connection_reuse_test"
	ctx := context.Background()

	_, err = ddb.CreateTableWithContext(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(tableName),
		KeySchema: []*dynamodb.KeySchemaElement{
			{AttributeName: aws.String("id"), KeyType: aws.String("HASH")},
		},
		AttributeDefinitions: []*dynamodb.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: aws.String("S")},
		},
		BillingMode: aws.String("PAY_PER_REQUEST"),
	})
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	var newConnCount atomic.Int32
	var reusedConnCount atomic.Int32
	var mu sync.Mutex
	seenConns := make(map[string]bool)

	numRequests := 20
	for i := 0; i < numRequests; i++ {
		trace := &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				mu.Lock()
				defer mu.Unlock()

				connKey := info.Conn.LocalAddr().String() + "->" + info.Conn.RemoteAddr().String()

				if info.Reused {
					reusedConnCount.Add(1)
					t.Logf("Request %d: Connection REUSED: %s", i, connKey)
				} else {
					if seenConns[connKey] {
						t.Logf("Request %d: RECONNECTION detected: %s", i, connKey)
					} else {
						newConnCount.Add(1)
						seenConns[connKey] = true
						t.Logf("Request %d: NEW connection: %s", i, connKey)
					}
				}
			},
		}

		traceCtx := httptrace.WithClientTrace(ctx, trace)

		_, err := ddb.GetItemWithContext(traceCtx, &dynamodb.GetItemInput{
			TableName: aws.String(tableName),
			Key: map[string]*dynamodb.AttributeValue{
				"id": {S: aws.String("test-id")},
			},
		})
		if err != nil {
			t.Fatalf("GetItem request %d error: %v", i, err)
		}

		time.Sleep(10 * time.Millisecond)
	}

	newConns := newConnCount.Load()
	reusedConns := reusedConnCount.Load()

	expectedMaxNewConns := int32(8)

	if newConns > expectedMaxNewConns {
		t.Errorf("Too many new connections created: %d (expected ≤ %d). "+
			"Check MaxIdleConnsPerHost setting.",
			newConns, expectedMaxNewConns)
	}

	minReusedConns := int32(numRequests / 2)
	if reusedConns < minReusedConns {
		t.Errorf("Too few connections reused: %d (expected ≥ %d). "+
			"Connection reuse rate: %.1f%%",
			reusedConns, minReusedConns, float64(reusedConns)/float64(numRequests)*100)
	}
}
