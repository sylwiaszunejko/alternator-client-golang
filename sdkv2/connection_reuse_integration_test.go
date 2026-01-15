//go:build integration
// +build integration

package sdkv2_test

import (
	"context"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	helper "github.com/scylladb/alternator-client-golang/sdkv2"
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

	_, err = ddb.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(tableName),
		KeySchema: []types.KeySchemaElement{
			{AttributeName: aws.String("id"), KeyType: types.KeyTypeHash},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("id"), AttributeType: types.ScalarAttributeTypeS},
		},
		BillingMode: types.BillingModePayPerRequest,
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
						// This is a reconnection (not first time seeing this local->remote pair)
						t.Logf("Request %d: RECONNECTION detected: %s (connection was closed)", i, connKey)
					} else {
						newConnCount.Add(1)
						seenConns[connKey] = true
						t.Logf("Request %d: NEW connection: %s", i, connKey)
					}
				}
			},
		}

		traceCtx := httptrace.WithClientTrace(ctx, trace)

		_, err := ddb.GetItem(traceCtx, &dynamodb.GetItemInput{
			TableName: aws.String(tableName),
			Key: map[string]types.AttributeValue{
				"id": &types.AttributeValueMemberS{Value: "test-id"},
			},
		})
		if err != nil {
			t.Fatalf("GetItem request %d error: %v", i, err)
		}

		time.Sleep(10 * time.Millisecond)
	}

	newConns := newConnCount.Load()
	reusedConns := reusedConnCount.Load()

	// After the first request establishes connections, most subsequent requests
	// should reuse connections. Allow for a few new connections due to
	// load balancing across multiple nodes.
	//
	// With 20 requests and 3 alternator nodes, we expect:
	// - ~3-6 new connections (one or two per node)
	// - ~14-17 reused connections
	expectedMaxNewConns := int32(8) // Allow some buffer

	if newConns > expectedMaxNewConns {
		t.Errorf("Too many new connections created: %d (expected ≤ %d). "+
			"This indicates connection reuse is not working properly. "+
			"Check MaxIdleConnsPerHost setting.",
			newConns, expectedMaxNewConns)
	}

	// At least half of the requests should reuse connections
	minReusedConns := int32(numRequests / 2)
	if reusedConns < minReusedConns {
		t.Errorf("Too few connections reused: %d (expected ≥ %d). "+
			"Connection reuse rate: %.1f%%. "+
			"This indicates a connection pooling problem.",
			reusedConns, minReusedConns, float64(reusedConns)/float64(numRequests)*100)
	}

	// Verify no excessive reconnections (connection closed and reopened to same remote)
	// This would indicate idle timeout is too short or connections are being closed prematurely
	if int32(len(seenConns)) > newConns {
		t.Errorf("Detected reconnections: saw %d unique connection pairs but only %d were 'new'. "+
			"This suggests connections are being closed and reopened.",
			len(seenConns), newConns)
	}
}
