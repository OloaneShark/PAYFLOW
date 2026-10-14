package payment

import (
	"context"
	"errors"
	"testing"
)

type stubStore struct {
	created Payment
	getID   string
}

func (s *stubStore) CreateOrGet(_ context.Context, p Payment) (Payment, error) {
	s.created = p
	return p, nil
}
func (s *stubStore) Get(_ context.Context, id string) (Payment, error) {
	s.getID = id
	return Payment{ID: id}, nil
}

func TestCreateValidation(t *testing.T) {
	tests := []struct {
		name string
		in   CreateInput
	}{
		{"zero amount", CreateInput{MerchantID: "merchant", IdempotencyKey: "key", Currency: "JPY"}},
		{"negative amount", CreateInput{MerchantID: "merchant", IdempotencyKey: "key", Amount: -1, Currency: "JPY"}},
		{"missing merchant", CreateInput{IdempotencyKey: "key", Amount: 100, Currency: "JPY"}},
		{"blank merchant", CreateInput{MerchantID: "  ", IdempotencyKey: "key", Amount: 100, Currency: "JPY"}},
		{"missing key", CreateInput{MerchantID: "merchant", Amount: 100, Currency: "JPY"}},
		{"blank key", CreateInput{MerchantID: "merchant", IdempotencyKey: " ", Amount: 100, Currency: "JPY"}},
		{"unsupported currency", CreateInput{MerchantID: "merchant", IdempotencyKey: "key", Amount: 100, Currency: "USD"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubStore{}
			_, err := NewService(store).Create(context.Background(), tc.in)
			if !errors.Is(err, ErrInvalidArgument) || store.created.ID != "" {
				t.Fatalf("Create() error = %v, store called = %t", err, store.created.ID != "")
			}
		})
	}
}

func TestCreateFingerprint(t *testing.T) {
	store := &stubStore{}
	svc := NewService(store)
	in := CreateInput{MerchantID: "merchant", IdempotencyKey: "key", Amount: 100, Currency: "JPY"}
	first, err := svc.Create(context.Background(), in)
	if err != nil || first.ID == "" || len(first.RequestFingerprint) != 32 {
		t.Fatalf("first Create() = %+v, %v", first, err)
	}
	second, err := svc.Create(context.Background(), in)
	if err != nil || string(first.RequestFingerprint) != string(second.RequestFingerprint) || first.ID == second.ID {
		t.Fatalf("second Create() = %+v, %v", second, err)
	}
	in.Amount++
	changed, err := svc.Create(context.Background(), in)
	if err != nil || string(first.RequestFingerprint) == string(changed.RequestFingerprint) {
		t.Fatalf("changed Create() = %+v, %v", changed, err)
	}
}

func TestGetValidation(t *testing.T) {
	store := &stubStore{}
	svc := NewService(store)
	if _, err := svc.Get(context.Background(), "not-a-uuid"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Get invalid ID error = %v", err)
	}
	const id = "123e4567-e89b-42d3-a456-426614174000"
	if _, err := svc.Get(context.Background(), id); err != nil || store.getID != id {
		t.Fatalf("Get valid ID error = %v, store ID = %q", err, store.getID)
	}
}
