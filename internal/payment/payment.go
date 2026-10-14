package payment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrConflict        = errors.New("idempotency key is already used for a different request")
	ErrNotFound        = errors.New("payment not found")
)

type Payment struct {
	ID                 string
	MerchantID         string
	IdempotencyKey     string
	RequestFingerprint []byte
	Amount             int64
	Currency           string
	Status             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type CreateInput struct {
	MerchantID     string
	IdempotencyKey string
	Amount         int64
	Currency       string
}

type Store interface {
	CreateOrGet(ctx context.Context, candidate Payment) (Payment, error)
	Get(ctx context.Context, id string) (Payment, error)
}

type Service struct{ store Store }

func NewService(store Store) *Service { return &Service{store: store} }

func (s *Service) Create(ctx context.Context, in CreateInput) (Payment, error) {
	if strings.TrimSpace(in.MerchantID) == "" || strings.TrimSpace(in.IdempotencyKey) == "" || in.Amount <= 0 || in.Currency != "JPY" {
		return Payment{}, ErrInvalidArgument
	}
	id, err := newUUID()
	if err != nil {
		return Payment{}, err
	}
	// Length prefixes avoid ambiguous concatenations. Only payload fields are hashed;
	// merchant and key are represented by the unique database constraint.
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(in.Amount))
	h.Write(n[:])
	binary.BigEndian.PutUint64(n[:], uint64(len(in.Currency)))
	h.Write(n[:])
	h.Write([]byte(in.Currency))
	return s.store.CreateOrGet(ctx, Payment{
		ID: id, MerchantID: in.MerchantID, IdempotencyKey: in.IdempotencyKey,
		RequestFingerprint: h.Sum(nil), Amount: in.Amount, Currency: in.Currency,
		Status: "CREATED",
	})
}

func (s *Service) Get(ctx context.Context, id string) (Payment, error) {
	if !validUUID(id) {
		return Payment{}, ErrInvalidArgument
	}
	return s.store.Get(ctx, id)
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:], nil
}

func validUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
