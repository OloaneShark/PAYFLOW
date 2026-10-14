package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/db/migrations"
	paymentv1 "payflow/gen/go/payflow/v1"
	paymentv1connect "payflow/gen/go/payflow/v1/payflowv1connect"
	"payflow/internal/payment"
	"payflow/internal/storage"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
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
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func testClient(t *testing.T) paymentv1connect.PaymentServiceClient {
	t.Helper()
	pool := testPool(t)
	server := connect.NewServer()
	paymentv1connect.RegisterPaymentServiceHandler(server, &payment.Handler{Service: payment.NewService(&storage.Postgres{Pool: pool})})
	return paymentv1connect.NewPaymentServiceClient(connect.NewClient(connectinprocess.New(server)))
}

func TestHTTPCreatePayment(t *testing.T) {
	pool := testPool(t)
	server := connect.NewServer()
	paymentv1connect.RegisterPaymentServiceHandler(server, &payment.Handler{Service: payment.NewService(&storage.Postgres{Pool: pool})})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	body := fmt.Sprintf(`{"merchantId":"http-%d","idempotencyKey":"key","amount":"1200","currency":"JPY"}`, time.Now().UnixNano())
	resp, err := http.Post(httpServer.URL+"/payflow.v1.PaymentService/CreatePayment", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP status = %d", resp.StatusCode)
	}
	var result struct {
		Payment struct {
			PaymentID string `json:"paymentId"`
			Amount    string `json:"amount"`
		} `json:"payment"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Payment.PaymentID == "" || result.Payment.Amount != "1200" {
		t.Fatalf("HTTP payment = %+v", result.Payment)
	}
}

func TestPaymentLifecycle(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	merchant := fmt.Sprintf("test-%d", time.Now().UnixNano())
	base := &paymentv1.CreatePaymentRequest{MerchantId: merchant, IdempotencyKey: "key", Amount: 100, Currency: "JPY"}
	created, err := client.CreatePayment(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	p := created.GetPayment()
	if p.GetPaymentId() == "" || p.GetMerchantId() != merchant || p.GetAmount() != 100 || p.GetCurrency() != "JPY" || p.GetStatus() != paymentv1.PaymentStatus_PAYMENT_STATUS_CREATED || p.GetCreatedAt() == nil || p.GetUpdatedAt() == nil {
		t.Fatalf("unexpected payment: %+v", p)
	}
	repeated, err := client.CreatePayment(ctx, base)
	if err != nil || repeated.GetPayment().GetPaymentId() != p.GetPaymentId() {
		t.Fatalf("repeat = %+v, %v", repeated, err)
	}
	changed := &paymentv1.CreatePaymentRequest{MerchantId: base.MerchantId, IdempotencyKey: base.IdempotencyKey, Amount: 101, Currency: base.Currency}
	if _, err := client.CreatePayment(ctx, changed); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("changed payload error = %v", err)
	}
	got, err := client.GetPayment(ctx, &paymentv1.GetPaymentRequest{PaymentId: p.GetPaymentId()})
	if err != nil || got.GetPayment().GetPaymentId() != p.GetPaymentId() {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := client.GetPayment(ctx, &paymentv1.GetPaymentRequest{PaymentId: "123e4567-e89b-42d3-a456-426614174000"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing payment error = %v", err)
	}
}

func TestInvalidCreateRequests(t *testing.T) {
	client := testClient(t)
	for _, tc := range []struct {
		name string
		edit func(*paymentv1.CreatePaymentRequest)
	}{
		{"zero amount", func(r *paymentv1.CreatePaymentRequest) { r.Amount = 0 }},
		{"negative amount", func(r *paymentv1.CreatePaymentRequest) { r.Amount = -1 }},
		{"missing merchant", func(r *paymentv1.CreatePaymentRequest) { r.MerchantId = "" }},
		{"missing key", func(r *paymentv1.CreatePaymentRequest) { r.IdempotencyKey = "" }},
		{"unsupported currency", func(r *paymentv1.CreatePaymentRequest) { r.Currency = "USD" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &paymentv1.CreatePaymentRequest{MerchantId: "merchant", IdempotencyKey: "key", Amount: 100, Currency: "JPY"}
			tc.edit(req)
			_, err := client.CreatePayment(context.Background(), req)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Fatalf("CreatePayment error = %v", err)
			}
		})
	}
}

func TestConcurrentSameKey(t *testing.T) {
	client := testClient(t)
	merchant := fmt.Sprintf("concurrent-%d", time.Now().UnixNano())
	const workers = 24
	var wg sync.WaitGroup
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := client.CreatePayment(context.Background(), &paymentv1.CreatePaymentRequest{
				MerchantId: merchant, IdempotencyKey: "same", Amount: 500, Currency: "JPY",
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- res.GetPayment().GetPaymentId()
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("different IDs: %q and %q", first, id)
		}
	}
	if first == "" {
		t.Fatal("no successful create")
	}
}
