package tests

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"payflow/internal/banking"
	"payflow/internal/ledger"
	"payflow/internal/payment"
	"payflow/internal/storage"
	"payflow/internal/verification"
)

func TestConcurrentProcessingBalanceAndFinancialAudit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := &storage.Postgres{Pool: pool}
	merchant := fmt.Sprintf("load-audit-%d", time.Now().UnixNano())
	service := payment.NewService(store)
	processor := payment.NewProcessor(store, &banking.Postgres{Pool: pool})
	first, err := service.Create(ctx, payment.CreateInput{MerchantID: merchant, IdempotencyKey: "repeat", Amount: 100, Currency: "JPY"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 128)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := service.Create(ctx, payment.CreateInput{MerchantID: merchant, IdempotencyKey: "repeat", Amount: 100, Currency: "JPY"})
			if err != nil || p.ID != first.ID {
				errorsCh <- fmt.Errorf("duplicate creation: %s %v", p.ID, err)
			}
		}()
	}
	wg.Wait()
	if _, err := service.Create(ctx, payment.CreateInput{MerchantID: merchant, IdempotencyKey: "repeat", Amount: 101, Currency: "JPY"}); !errors.Is(err, payment.ErrConflict) {
		t.Fatalf("changed same-key payload: %v", err)
	}
	for i := 0; i < 12; i++ {
		p, err := service.Create(ctx, payment.CreateInput{MerchantID: merchant, IdempotencyKey: fmt.Sprintf("parallel-%d", i), Amount: 100, Currency: "JPY"})
		if err != nil {
			t.Fatal(err)
		}
		scenario := banking.Approve
		if i%3 == 0 {
			scenario = banking.Decline
		}
		for j := 0; j < 3; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := processor.Process(ctx, p.ID, scenario); err != nil {
					errorsCh <- err
				}
			}()
		}
	}
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := (&ledger.Service{Pool: pool}).Balance(ctx, "MERCHANT_PAYABLE", merchant, "JPY")
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				errorsCh <- err
			}
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	report, err := verification.Check(ctx, pool, merchant)
	if err != nil || !report.Passed || report.Completed != 8 || report.Declined != 4 || report.Journals != 8 || report.ExpectedPayable != 800 || report.ActualPayable != 800 {
		t.Fatalf("financial audit: %+v %v", report, err)
	}
}
