package tests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"payflow/internal/admin"
	"payflow/internal/auth"
)

type testSecretWriter struct {
	calls int
	name  string
	value string
	err   error
}

func (w *testSecretWriter) Create(_ context.Context, name, value string) (string, error) {
	w.calls++
	w.name, w.value = name, value
	if w.err != nil {
		return "", w.err
	}
	return "arn:aws:secretsmanager:us-east-1:123456789012:secret:" + name + "-sample", nil
}

func adminRequestID() string {
	return fmt.Sprintf("123e4567-e89b-42d3-a456-%012x", time.Now().UnixNano()&0xffffffffffff)
}

func TestPrivateCredentialLifecycleAndFailureSafety(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	writer := &testSecretWriter{}
	svc := &admin.CredentialService{Pool: pool, Writer: writer, Prefix: "payflow-test/merchant-api-key"}
	keys := &auth.Store{Pool: pool}
	merchant := fmt.Sprintf("admin-merchant-%d", time.Now().UnixNano())
	requestID := adminRequestID()
	if _, err := svc.Issue(ctx, merchant, requestID, "test-admin", false); !errors.Is(err, admin.ErrInvalidRequest) {
		t.Fatalf("unconfirmed issue: %v", err)
	}
	if _, err := svc.Issue(ctx, "bad;command", requestID, "test-admin", true); !errors.Is(err, admin.ErrInvalidRequest) {
		t.Fatalf("unsafe merchant ID: %v", err)
	}
	issued, err := svc.Issue(ctx, merchant, requestID, "test-admin", true)
	if err != nil || issued.State != "SUCCEEDED" || issued.SecretARN == "" || issued.NewKeyID == "" {
		t.Fatalf("issue: %+v %v", issued, err)
	}
	if writer.calls != 1 || writer.name != svc.Prefix+"/"+requestID || !strings.HasPrefix(writer.value, "pfk_") {
		t.Fatal("key was not delivered once to the deterministic secret name")
	}
	if who, err := keys.Authenticate(ctx, writer.value); err != nil || who != merchant {
		t.Fatalf("issued key inactive: %q %v", who, err)
	}
	encoded, _ := json.Marshal(issued)
	if strings.Contains(string(encoded), writer.value) {
		t.Fatal("administrative output exposed the API key")
	}
	var auditRecord string
	if err := pool.QueryRow(ctx, `SELECT row_to_json(o)::text FROM credential_admin_operations o WHERE request_id=$1`, requestID).Scan(&auditRecord); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(auditRecord, writer.value) {
		t.Fatal("credential audit exposed the API key")
	}
	if replay, err := svc.Issue(ctx, merchant, requestID, "test-admin", true); err != nil || replay.NewKeyID != issued.NewKeyID || writer.calls != 1 {
		t.Fatalf("duplicate issue: %+v %v, deliveries=%d", replay, err, writer.calls)
	}
	if _, err := svc.Issue(ctx, merchant+"-changed", requestID, "test-admin", true); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("conflicting request: %v", err)
	}

	oldKey := writer.value
	writer.err = errors.New("injected secret delivery failure")
	failedRequest := adminRequestID()
	if _, err := svc.Rotate(ctx, issued.NewKeyID, failedRequest, "test-admin", true); !errors.Is(err, admin.ErrNeedsReview) {
		t.Fatalf("failed delivery: %v", err)
	}
	if who, err := keys.Authenticate(ctx, oldKey); err != nil || who != merchant {
		t.Fatalf("old key lost after delivery failure: %q %v", who, err)
	}
	failed, err := svc.Get(ctx, failedRequest)
	if err != nil || failed.State != "NEEDS_REVIEW" {
		t.Fatalf("failed audit: %+v %v", failed, err)
	}
	var active bool
	if err := pool.QueryRow(ctx, `SELECT activated_at IS NOT NULL FROM merchant_api_keys WHERE id=$1`, failed.NewKeyID).Scan(&active); err != nil || active {
		t.Fatalf("failed delivery activated key: %v %v", active, err)
	}
	if _, err := svc.Rotate(ctx, issued.NewKeyID, failedRequest, "test-admin", true); !errors.Is(err, admin.ErrNeedsReview) {
		t.Fatalf("uncertain retry should not create a second key: %v", err)
	}

	writer.err = nil
	rotated, err := svc.Rotate(ctx, issued.NewKeyID, adminRequestID(), "test-admin", true)
	if err != nil || rotated.State != "SUCCEEDED" {
		t.Fatalf("rotation: %+v %v", rotated, err)
	}
	if _, err := keys.Authenticate(ctx, oldKey); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old key survived successful rotation: %v", err)
	}
	if who, err := keys.Authenticate(ctx, writer.value); err != nil || who != merchant {
		t.Fatalf("successor unavailable: %q %v", who, err)
	}
	revokeID := adminRequestID()
	revoked, err := svc.Revoke(ctx, rotated.NewKeyID, revokeID, "test-admin", true)
	if err != nil || revoked.State != "SUCCEEDED" {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, err := keys.Authenticate(ctx, writer.value); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("revoked key active: %v", err)
	}
	if replay, err := svc.Revoke(ctx, rotated.NewKeyID, revokeID, "test-admin", true); err != nil || replay.State != "SUCCEEDED" {
		t.Fatalf("idempotent revoke: %+v %v", replay, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM credential_admin_operations WHERE request_id=$1`, requestID); err == nil {
		t.Fatal("credential audit record was deleted")
	}
}
