package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"payflow/internal/payment"
)

var (
	ErrInvalidRequest = errors.New("invalid administrative request")
	ErrNeedsReview    = errors.New("credential operation needs private review; do not retry with a new request ID")
	ErrConflict       = errors.New("administrative request ID already has different instructions")
)

var adminMerchantID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// SecretWriter delivers new key material without returning it to task output.
type SecretWriter interface {
	Create(context.Context, string, string) (string, error)
}

type CredentialService struct {
	Pool   *pgxpool.Pool
	Writer SecretWriter
	Prefix string
}

type Operation struct {
	RequestID string `json:"request_id"`
	Action    string `json:"action"`
	State     string `json:"state"`
	Merchant  string `json:"merchant_id,omitempty"`
	TargetID  string `json:"target_key_id,omitempty"`
	NewKeyID  string `json:"new_key_id,omitempty"`
	SecretARN string `json:"secret_arn,omitempty"`
}

func (s *CredentialService) Get(ctx context.Context, requestID string) (Operation, error) {
	if !payment.ValidPaymentID(requestID) {
		return Operation{}, ErrInvalidRequest
	}
	var op Operation
	var merchant, target, newID, secretARN *string
	err := s.Pool.QueryRow(ctx, `SELECT request_id,action,state,merchant_id,target_key_id,new_key_id,secret_arn
		FROM credential_admin_operations WHERE request_id=$1`, requestID).
		Scan(&op.RequestID, &op.Action, &op.State, &merchant, &target, &newID, &secretARN)
	if err != nil {
		return Operation{}, err
	}
	if merchant != nil {
		op.Merchant = *merchant
	}
	if target != nil {
		op.TargetID = *target
	}
	if newID != nil {
		op.NewKeyID = *newID
	}
	if secretARN != nil {
		op.SecretARN = *secretARN
	}
	return op, nil
}

func (s *CredentialService) Issue(ctx context.Context, merchant, requestID, actor string, confirm bool) (Operation, error) {
	if !adminMerchantID.MatchString(merchant) {
		return Operation{}, ErrInvalidRequest
	}
	return s.provision(ctx, "ISSUE", merchant, "", requestID, actor, confirm)
}

func (s *CredentialService) Rotate(ctx context.Context, targetID, requestID, actor string, confirm bool) (Operation, error) {
	if !payment.ValidPaymentID(targetID) {
		return Operation{}, ErrInvalidRequest
	}
	return s.provision(ctx, "ROTATE", "", targetID, requestID, actor, confirm)
}

func (s *CredentialService) provision(ctx context.Context, action, merchant, targetID, requestID, actor string, confirm bool) (Operation, error) {
	if !confirm || !payment.ValidPaymentID(requestID) || strings.TrimSpace(actor) == "" || s.Pool == nil || s.Writer == nil || s.Prefix == "" {
		return Operation{}, ErrInvalidRequest
	}
	if prior, err := s.Get(ctx, requestID); err == nil {
		if prior.Action != action || prior.TargetID != targetID || (merchant != "" && prior.Merchant != merchant) {
			return Operation{}, ErrConflict
		}
		if prior.State != "SUCCEEDED" {
			return prior, ErrNeedsReview
		}
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, err
	}
	newID, key, digest, err := newCredential()
	if err != nil {
		return Operation{}, err
	}
	secretName := s.Prefix + "/" + requestID
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback(ctx)
	if action == "ROTATE" {
		err = tx.QueryRow(ctx, `SELECT merchant_id FROM merchant_api_keys
			WHERE id=$1 AND revoked_at IS NULL AND activated_at IS NOT NULL FOR UPDATE`, targetID).Scan(&merchant)
		if errors.Is(err, pgx.ErrNoRows) {
			return Operation{}, ErrInvalidRequest
		}
		if err != nil {
			return Operation{}, err
		}
	}
	var inserted string
	err = tx.QueryRow(ctx, `INSERT INTO credential_admin_operations
		(request_id,action,actor,merchant_id,target_key_id,new_key_id,secret_name,state)
		VALUES ($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,'PREPARED')
		ON CONFLICT (request_id) DO NOTHING RETURNING request_id`,
		requestID, action, actor, merchant, targetID, newID, secretName).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return s.existing(ctx, requestID, action, merchant, targetID)
	}
	if err != nil {
		return Operation{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO merchant_api_keys
		(id,merchant_id,key_hash,activated_at,admin_request_id)
		VALUES ($1,$2,$3,NULL,$4)`, newID, merchant, digest[:], requestID)
	if err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, err
	}

	arn, err := s.Writer.Create(ctx, secretName, key)
	if err != nil || arn == "" {
		s.markReview(requestID, "secret_delivery")
		return Operation{}, ErrNeedsReview
	}
	result, err := s.activate(ctx, requestID, action, targetID, newID, arn)
	if err != nil {
		s.markReview(requestID, "database_activation")
		return Operation{}, ErrNeedsReview
	}
	return result, nil
}

func (s *CredentialService) activate(ctx context.Context, requestID, action, targetID, newID, arn string) (Operation, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback(ctx)
	if action == "ROTATE" {
		tag, err := tx.Exec(ctx, `UPDATE merchant_api_keys SET revoked_at=now()
			WHERE id=$1 AND revoked_at IS NULL AND activated_at IS NOT NULL`, targetID)
		if err != nil {
			return Operation{}, err
		}
		if tag.RowsAffected() != 1 {
			return Operation{}, ErrNeedsReview
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE merchant_api_keys SET activated_at=now()
		WHERE id=$1 AND admin_request_id=$2 AND activated_at IS NULL`, newID, requestID)
	if err != nil {
		return Operation{}, err
	}
	if tag.RowsAffected() != 1 {
		return Operation{}, ErrNeedsReview
	}
	tag, err = tx.Exec(ctx, `UPDATE credential_admin_operations
		SET state='SUCCEEDED',secret_arn=$2,finished_at=now()
		WHERE request_id=$1 AND state='PREPARED'`, requestID, arn)
	if err != nil {
		return Operation{}, err
	}
	if tag.RowsAffected() != 1 {
		return Operation{}, ErrNeedsReview
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, err
	}
	return s.Get(ctx, requestID)
}

func (s *CredentialService) markReview(requestID, category string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.Pool.Exec(ctx, `UPDATE credential_admin_operations
		SET state='NEEDS_REVIEW',error_category=$2,finished_at=now()
		WHERE request_id=$1 AND state='PREPARED'`, requestID, category)
}

func (s *CredentialService) existing(ctx context.Context, requestID, action, merchant, targetID string) (Operation, error) {
	op, err := s.Get(ctx, requestID)
	if err != nil {
		return Operation{}, err
	}
	if op.Action != action || op.TargetID != targetID || (merchant != "" && op.Merchant != merchant) {
		return Operation{}, ErrConflict
	}
	if op.State != "SUCCEEDED" {
		return op, ErrNeedsReview
	}
	return op, nil
}

func (s *CredentialService) Revoke(ctx context.Context, targetID, requestID, actor string, confirm bool) (Operation, error) {
	if !confirm || !payment.ValidPaymentID(targetID) || !payment.ValidPaymentID(requestID) || strings.TrimSpace(actor) == "" || s.Pool == nil {
		return Operation{}, ErrInvalidRequest
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback(ctx)
	var merchant string
	err = tx.QueryRow(ctx, `SELECT merchant_id FROM merchant_api_keys
		WHERE id=$1 AND revoked_at IS NULL AND activated_at IS NOT NULL FOR UPDATE`, targetID).Scan(&merchant)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		if op, checkErr := s.Get(ctx, requestID); checkErr == nil && op.Action == "REVOKE" && op.TargetID == targetID && op.State == "SUCCEEDED" {
			return op, nil
		}
		return Operation{}, ErrInvalidRequest
	}
	if err != nil {
		return Operation{}, err
	}
	var inserted string
	err = tx.QueryRow(ctx, `INSERT INTO credential_admin_operations
		(request_id,action,actor,merchant_id,target_key_id,state)
		VALUES ($1,'REVOKE',$2,$3,$4,'PREPARED')
		ON CONFLICT (request_id) DO NOTHING RETURNING request_id`, requestID, actor, merchant, targetID).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Rollback(ctx)
		return s.existing(ctx, requestID, "REVOKE", merchant, targetID)
	}
	if err != nil {
		return Operation{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE merchant_api_keys SET revoked_at=now() WHERE id=$1`, targetID)
	if err != nil {
		return Operation{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE credential_admin_operations
		SET state='SUCCEEDED',finished_at=now() WHERE request_id=$1`, requestID)
	if err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Operation{}, err
	}
	return s.Get(ctx, requestID)
}

func newCredential() (id, key string, digest [32]byte, err error) {
	var rawID [16]byte
	var secret [32]byte
	if _, err = rand.Read(rawID[:]); err != nil {
		return "", "", digest, err
	}
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", digest, err
	}
	rawID[6] = rawID[6]&0x0f | 0x40
	rawID[8] = rawID[8]&0x3f | 0x80
	h := hex.EncodeToString(rawID[:])
	id = fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:])
	key = "pfk_" + hex.EncodeToString(secret[:])
	digest = sha256.Sum256([]byte(key))
	return id, key, digest, nil
}
