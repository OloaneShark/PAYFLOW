package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/db/migrations"
	paymentv1connect "payflow/gen/go/payflow/v1/payflowv1connect"
	"payflow/internal/auth"
	"payflow/internal/ledger"
	"payflow/internal/observability"
	"payflow/internal/orchestration"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func TestConnectHTTPMerchantAuthentication(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	keys := &auth.Store{Pool: pool}
	merchant := fmt.Sprintf("http-secure-%d", time.Now().UnixNano())
	id, key, err := keys.Issue(ctx, merchant)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := keys.Issue(ctx, merchant+"-other")
	if err != nil {
		t.Fatal(err)
	}
	server := connect.NewServer()
	store := &storage.Postgres{Pool: pool}
	paymentv1connect.RegisterPaymentServiceHandler(server, &payment.Handler{Service: payment.NewService(store), Submitter: &orchestration.SubmissionService{Store: store}, Ledger: &ledger.Service{Pool: pool}, AuthRequired: true})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	api := httptest.NewServer(apiMiddleware(mux, keys, observability.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second))
	defer api.Close()
	call := func(method, token, body string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, api.URL+"/payflow.v1.PaymentService/"+method, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := api.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var data map[string]any
		_ = json.NewDecoder(res.Body).Decode(&data)
		return res.StatusCode, data
	}
	body := `{"idempotencyKey":"secure-http","amount":"500","currency":"JPY"}`
	if status, _ := call("CreatePayment", "", body); status != 401 {
		t.Fatalf("missing key %d", status)
	}
	if status, _ := call("CreatePayment", "pfk_invalid", body); status != 401 {
		t.Fatalf("invalid key %d", status)
	}
	status, data := call("CreatePayment", key, body)
	if status != 200 {
		t.Fatalf("valid key %d: %+v", status, data)
	}
	record, ok := data["payment"].(map[string]any)
	if !ok {
		t.Fatalf("payment: %+v", data)
	}
	paymentID, _ := record["paymentId"].(string)
	if paymentID == "" || record["merchantId"] != merchant {
		t.Fatalf("ownership: %+v", record)
	}
	getBody := fmt.Sprintf(`{"paymentId":%q}`, paymentID)
	if status, _ := call("SubmitPayment", "", getBody); status != 401 {
		t.Fatalf("missing key on submission %d", status)
	}
	if status, _ := call("SubmitPayment", otherKey, getBody); status != 404 {
		t.Fatalf("cross merchant submission %d", status)
	}
	if status, _ := call("SubmitPayment", key, getBody); status != 200 {
		t.Fatalf("valid submission %d", status)
	}
	if status, _ := call("SubmitPayment", key, getBody); status != 200 {
		t.Fatalf("duplicate submission %d", status)
	}
	if status, _ := call("GetPayment", otherKey, getBody); status != 404 {
		t.Fatalf("cross merchant get %d", status)
	}
	if status, _ := call("CreatePayment", otherKey, fmt.Sprintf(`{"merchantId":%q,"idempotencyKey":"spoof","amount":"500","currency":"JPY"}`, merchant)); status != 403 {
		t.Fatalf("spoofed merchant %d", status)
	}
	if status, _ := call("GetMerchantBalance", otherKey, `{"currency":"JPY"}`); status != 200 {
		t.Fatalf("isolated balance %d", status)
	}
	nextID, nextKey, err := keys.Rotate(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := call("GetPayment", key, getBody); status != 401 {
		t.Fatalf("rotated old key %d", status)
	}
	if status, _ := call("SubmitPayment", key, getBody); status != 401 {
		t.Fatalf("rotated old key submitted %d", status)
	}
	if status, _ := call("GetPayment", nextKey, getBody); status != 200 {
		t.Fatalf("rotated new key %d", status)
	}
	if err := keys.Revoke(ctx, nextID); err != nil {
		t.Fatal(err)
	}
	if status, _ := call("GetPayment", nextKey, getBody); status != 401 {
		t.Fatalf("revoked key %d", status)
	}
	if status, _ := call("SubmitPayment", nextKey, getBody); status != 401 {
		t.Fatalf("revoked key submitted %d", status)
	}
}
