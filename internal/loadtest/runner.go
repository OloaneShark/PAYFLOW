package loadtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"
	paymentv1 "payflow/gen/go/payflow/v1"
	paymentv1connect "payflow/gen/go/payflow/v1/payflowv1connect"
	"payflow/internal/auth"
	"payflow/internal/orchestration"
	"payflow/internal/verification"
)

type caller struct {
	client paymentv1connect.PaymentServiceClient
	key    string
	limit  time.Duration
}

func (c caller) context(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, c.limit)
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+c.key)
	return ctx, cancel
}

func (c caller) create(parent context.Context, merchant, idempotencyKey string) (string, error) {
	ctx, cancel := c.context(parent)
	defer cancel()
	response, err := c.client.CreatePayment(ctx, &paymentv1.CreatePaymentRequest{
		MerchantId: merchant, IdempotencyKey: idempotencyKey, Amount: 100, Currency: "JPY",
	})
	if err != nil {
		return "", err
	}
	if response.GetPayment().GetPaymentId() == "" {
		return "", errors.New("empty payment creation result")
	}
	return response.GetPayment().GetPaymentId(), nil
}

func (c caller) get(parent context.Context, id string) error {
	ctx, cancel := c.context(parent)
	defer cancel()
	_, err := c.client.GetPayment(ctx, &paymentv1.GetPaymentRequest{PaymentId: id})
	return err
}

func (c caller) submit(parent context.Context, id string) error {
	ctx, cancel := c.context(parent)
	defer cancel()
	_, err := c.client.SubmitPayment(ctx, &paymentv1.SubmitPaymentRequest{PaymentId: id})
	return err
}

func (c caller) balance(parent context.Context) error {
	ctx, cancel := c.context(parent)
	defer cancel()
	_, err := c.client.GetMerchantBalance(ctx, &paymentv1.GetMerchantBalanceRequest{Currency: "JPY"})
	return err
}

func preflight(ctx context.Context, httpClient *http.Client, api, database string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, api+"/healthz", nil)
	if err != nil {
		return err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Payflow-Loadtest-Database") != database {
		return errors.New("API is not advertising the dedicated local load test database")
	}
	return nil
}

func Run(ctx context.Context, config Config) (Result, error) {
	if err := config.Defaults(); err != nil {
		return Result{}, err
	}
	database, err := DatabaseName(config.DatabaseURL)
	if err != nil {
		return Result{}, err
	}
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		return Result{}, err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return Result{}, err
	}
	transport := &http.Transport{MaxConnsPerHost: config.Clients, MaxIdleConnsPerHost: config.Clients, MaxIdleConns: config.Clients, IdleConnTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport, Timeout: config.RequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	if err := preflight(ctx, httpClient, config.API, database); err != nil {
		return Result{}, err
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Result{}, err
	}
	runID := hex.EncodeToString(random[:])
	merchant := "loadtest-" + runID
	keys := &auth.Store{Pool: pool}
	credentialID, key, err := keys.Issue(ctx, merchant)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = keys.Revoke(cleanupCtx, credentialID)
	}()
	client := paymentv1connect.NewPaymentServiceClient(connect.NewClient(connecthttp.NewTransport(httpClient, config.API)))
	api := caller{client: client, key: key, limit: config.RequestTimeout}
	setupCtx, stopSetup := context.WithTimeout(ctx, 2*time.Minute)
	defer stopSetup()
	var postgresVersion string
	if err := pool.QueryRow(ctx, `SHOW server_version`).Scan(&postgresVersion); err != nil {
		return Result{}, err
	}
	seed := ""
	if config.Workload == "get" || config.Workload == "concurrent-submit" || config.Workload == "mixed" {
		seed, err = api.create(setupCtx, merchant, runID+"-seed")
		if err != nil {
			return Result{}, err
		}
	}
	ids := make([]string, 0)
	if config.Workload == "submit" {
		ids = make([]string, config.Requests)
		for i := range ids {
			ids[i], err = api.create(setupCtx, merchant, fmt.Sprintf("%s-setup-%d", runID, i))
			if err != nil {
				return Result{}, err
			}
		}
	}
	result := Result{
		TestName: config.Preset + "/" + config.Workload, DateUTC: time.Now().UTC(),
		PostgresVersion: postgresVersion, TemporalVersion: config.TemporalVersion,
		Concurrency: config.Clients, Operations: make(map[string]int), ErrorCategories: make(map[string]int),
		RequestLimit: config.Requests, DurationLimitSec: config.Duration.Seconds(),
		Recovery:    "not injected by this run",
		Limitations: []string{"API preparation, warmup, settlement, and invariant checks are excluded from measured request latency", "Temporal version is a configured Compose image unless independently supplied", "Workflow history is sampled from at most 100 terminal dispatches; DB operation and ledger posting latencies are not individually instrumented"},
	}
	if err := warmup(ctx, api, config); err != nil {
		return Result{}, err
	}
	var next atomic.Int64
	var mu sync.Mutex
	samples := make([]time.Duration, 0, config.Requests)
	submissionSamples := make([]time.Duration, 0, config.Requests/4)
	started := time.Now()
	var wg sync.WaitGroup
	for worker := 0; worker < config.Clients; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lastID := seed
			for {
				if ctx.Err() != nil || (config.Duration > 0 && time.Since(started) >= config.Duration) {
					return
				}
				index := int(next.Add(1)) - 1
				if index >= config.Requests {
					return
				}
				if config.Preset == "sustained" {
					startAt := started.Add(time.Duration(index) * config.Duration / time.Duration(config.Requests))
					if wait := time.Until(startAt); wait > 0 {
						timer := time.NewTimer(wait)
						select {
						case <-ctx.Done():
							timer.Stop()
							return
						case <-timer.C:
						}
					}
					if time.Since(started) >= config.Duration {
						return
					}
				}
				operation := config.Workload
				if operation == "mixed" {
					operation = []string{"create", "get", "submit", "balance"}[index%4]
				}
				begin := time.Now()
				var callErr error
				switch operation {
				case "create":
					lastID, callErr = api.create(ctx, merchant, fmt.Sprintf("%s-create-%d", runID, index))
				case "repeat":
					_, callErr = api.create(ctx, merchant, runID+"-repeat")
				case "get":
					callErr = api.get(ctx, lastID)
				case "submit":
					id := lastID
					if config.Workload == "submit" {
						id = ids[index]
					}
					callErr = api.submit(ctx, id)
				case "concurrent-submit":
					callErr = api.submit(ctx, seed)
				case "balance":
					callErr = api.balance(ctx)
				}
				duration := time.Since(begin)
				mu.Lock()
				samples = append(samples, duration)
				if operation == "submit" || operation == "concurrent-submit" {
					submissionSamples = append(submissionSamples, duration)
				}
				result.Operations[operation]++
				result.TotalRequests++
				if callErr == nil {
					result.Successful++
				} else {
					result.Failed++
					category := connect.CodeOf(callErr).String()
					if errors.Is(callErr, context.DeadlineExceeded) {
						category = "deadline_exceeded"
					}
					result.ErrorCategories[category]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	result.DurationSeconds = time.Since(started).Seconds()
	result.Latency = latency(samples)
	result.Submission = latency(submissionSamples)
	result.WarmupSeconds = config.Warmup.Seconds()
	if result.DurationSeconds > 0 {
		result.RequestsPerSec = float64(result.TotalRequests) / result.DurationSeconds
	}
	if err := settle(ctx, pool, merchant, config.SettleTimeout); err != nil {
		return result, err
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payments p JOIN payment_workflow_dispatch d ON d.payment_id=p.id
		WHERE p.merchant_id=$1 AND p.status NOT IN ('COMPLETED','FAILED')`, merchant).Scan(&result.PendingSubmitted); err != nil {
		return result, err
	}
	result.Completion, result.Dispatch, result.Reconciliation, result.Terminal, result.ProcessingWindow, err = asyncTimings(ctx, pool, merchant)
	if err != nil {
		return result, err
	}
	result.Invariant, err = verification.Check(ctx, pool, merchant)
	if err != nil {
		return result, err
	}
	if err := workloadChecks(ctx, pool, merchant, runID, config.Workload, seed, &result); err != nil {
		return result, err
	}
	result.Workflow, result.WorkflowCandidates, err = workflowTimings(ctx, pool, merchant, config)
	if err != nil {
		result.Limitations = append(result.Limitations, "Local Temporal workflow history was unavailable for this run")
	} else if result.Workflow.Count < result.WorkflowCandidates {
		result.Limitations = append(result.Limitations, "Some sampled terminal payments had no closed Temporal workflow history at observation time")
	}
	result.Passed = result.TotalRequests > 0 && result.Failed == 0 && result.PendingSubmitted == 0 && result.Invariant.Passed && ctx.Err() == nil
	if config.RecoveryScenario != "" {
		if result.Passed {
			result.Recovery = config.RecoveryScenario + ": caller-supplied label; injection not verified by client; submissions settled and invariants passed"
		} else {
			result.Recovery = config.RecoveryScenario + ": caller-supplied label; injection not verified by client; incomplete or failed"
		}
	}
	result.runtimeMetadata()
	return result, nil
}

func workflowTimings(ctx context.Context, pool *pgxpool.Pool, merchant string, config Config) (AsyncTiming, int, error) {
	rows, err := pool.Query(ctx, `SELECT d.workflow_id FROM payment_workflow_dispatch d
		JOIN payments p ON p.id=d.payment_id WHERE p.merchant_id=$1 AND p.status IN ('COMPLETED','FAILED')
		ORDER BY d.requested_at LIMIT 100`, merchant)
	if err != nil {
		return AsyncTiming{}, 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return AsyncTiming{}, 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) == 0 {
		return AsyncTiming{}, 0, err
	}
	sampleCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	temporalClient, err := client.DialContext(sampleCtx, client.Options{HostPort: config.TemporalAddress, Namespace: config.TemporalNS})
	if err != nil {
		return AsyncTiming{}, len(ids), err
	}
	defer temporalClient.Close()
	var timing AsyncTiming
	for _, id := range ids {
		if sampleCtx.Err() != nil {
			break
		}
		requestCtx, cancel := context.WithTimeout(sampleCtx, time.Second)
		response, describeErr := temporalClient.DescribeWorkflowExecution(requestCtx, id, "")
		cancel()
		if describeErr != nil || response == nil || response.GetWorkflowExecutionInfo() == nil {
			continue
		}
		info := response.GetWorkflowExecutionInfo()
		started, closed := info.GetStartTime(), info.GetCloseTime()
		if started == nil || closed == nil || !started.IsValid() || !closed.IsValid() {
			continue
		}
		elapsed := closed.AsTime().Sub(started.AsTime())
		if elapsed < 0 {
			continue
		}
		timing.Count++
		timing.AverageMS += float64(elapsed) / float64(time.Millisecond)
	}
	if timing.Count > 0 {
		timing.AverageMS /= float64(timing.Count)
	}
	return timing, len(ids), nil
}

func warmup(ctx context.Context, api caller, config Config) error {
	if config.Warmup == 0 {
		return nil
	}
	warmCtx, cancel := context.WithTimeout(ctx, config.Warmup)
	defer cancel()
	var next atomic.Int64
	var first error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < config.Clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for warmCtx.Err() == nil && next.Add(1) <= 1000 {
				if err := api.balance(warmCtx); err != nil && warmCtx.Err() == nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	if first == nil && ctx.Err() == nil {
		<-warmCtx.Done()
	}
	return first
}

func settle(ctx context.Context, pool *pgxpool.Pool, merchant string, limit time.Duration) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		var pending int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM payments p JOIN payment_workflow_dispatch d ON d.payment_id=p.id
			WHERE p.merchant_id=$1 AND p.status NOT IN ('COMPLETED','FAILED')`, merchant).Scan(&pending)
		if err == nil && pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if err != nil {
				return err
			}
			return nil // Report pending work as an incomplete benchmark.
		case <-ticker.C:
		}
	}
}

func asyncTimings(ctx context.Context, pool *pgxpool.Pool, merchant string) (AsyncTiming, AsyncTiming, AsyncTiming, AsyncTiming, AsyncTiming, error) {
	var completion, dispatch, reconciliation, terminal, processing AsyncTiming
	err := pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE p.completed_at IS NOT NULL),
		COALESCE(avg(EXTRACT(EPOCH FROM p.completed_at-p.created_at)*1000) FILTER (WHERE p.completed_at IS NOT NULL),0)::float8,
		count(*) FILTER (WHERE d.dispatched_at IS NOT NULL),
		COALESCE(avg(EXTRACT(EPOCH FROM d.dispatched_at-d.requested_at)*1000) FILTER (WHERE d.dispatched_at IS NOT NULL),0)::float8,
		count(*) FILTER (WHERE p.completed_at IS NOT NULL AND p.reconciliation_required_at IS NOT NULL),
		COALESCE(avg(EXTRACT(EPOCH FROM p.completed_at-p.reconciliation_required_at)*1000)
			FILTER (WHERE p.completed_at IS NOT NULL AND p.reconciliation_required_at IS NOT NULL),0)::float8,
		count(*) FILTER (WHERE COALESCE(p.completed_at,p.failed_at) IS NOT NULL AND d.requested_at IS NOT NULL),
		COALESCE(avg(EXTRACT(EPOCH FROM COALESCE(p.completed_at,p.failed_at)-d.requested_at)*1000)
			FILTER (WHERE COALESCE(p.completed_at,p.failed_at) IS NOT NULL AND d.requested_at IS NOT NULL),0)::float8,
		count(*) FILTER (WHERE COALESCE(p.completed_at,p.failed_at) IS NOT NULL AND p.processing_started_at IS NOT NULL),
		COALESCE(avg(EXTRACT(EPOCH FROM COALESCE(p.completed_at,p.failed_at)-p.processing_started_at)*1000)
			FILTER (WHERE COALESCE(p.completed_at,p.failed_at) IS NOT NULL AND p.processing_started_at IS NOT NULL),0)::float8
		FROM payments p LEFT JOIN payment_workflow_dispatch d ON d.payment_id=p.id
		WHERE p.merchant_id=$1`, merchant).Scan(&completion.Count, &completion.AverageMS,
		&dispatch.Count, &dispatch.AverageMS, &reconciliation.Count, &reconciliation.AverageMS,
		&terminal.Count, &terminal.AverageMS, &processing.Count, &processing.AverageMS)
	return completion, dispatch, reconciliation, terminal, processing, err
}

func workloadChecks(ctx context.Context, pool *pgxpool.Pool, merchant, runID, workload, seed string, result *Result) error {
	var count int
	switch workload {
	case "repeat":
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM payments WHERE merchant_id=$1 AND idempotency_key=$2`, merchant, runID+"-repeat").Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			result.Invariant.ViolationCount++
			result.Invariant.Violations = append(result.Invariant.Violations, "repeated idempotency key did not produce exactly one payment")
		}
	case "concurrent-submit":
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_workflow_dispatch WHERE payment_id=$1 AND workflow_id=$2`, seed, orchestration.WorkflowID(seed)).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			result.Invariant.ViolationCount++
			result.Invariant.Violations = append(result.Invariant.Violations, "concurrent submissions did not retain one stable workflow ID")
		}
	}
	result.Invariant.Passed = result.Invariant.ViolationCount == 0
	return nil
}
