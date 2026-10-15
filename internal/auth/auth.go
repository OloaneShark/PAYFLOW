package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnauthenticated = errors.New("invalid API credential")

type identityKey struct{}

func WithMerchant(ctx context.Context, merchant string) context.Context {
	return context.WithValue(ctx, identityKey{}, merchant)
}

func Merchant(ctx context.Context) (string, bool) {
	merchant, ok := ctx.Value(identityKey{}).(string)
	return merchant, ok && merchant != ""
}

type Store struct{ Pool *pgxpool.Pool }

// Issue returns a key exactly once. Its 256 random bits make a fast digest
// appropriate here; no password or user-chosen low-entropy secret is accepted.
func (s *Store) Issue(ctx context.Context, merchant string) (id, key string, err error) {
	if strings.TrimSpace(merchant) == "" || merchant != strings.TrimSpace(merchant) {
		return "", "", errors.New("merchant ID is required")
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	var rawID [16]byte
	if _, err = rand.Read(rawID[:]); err != nil {
		return "", "", err
	}
	rawID[6] = rawID[6]&0x0f | 0x40
	rawID[8] = rawID[8]&0x3f | 0x80
	h := hex.EncodeToString(rawID[:])
	id = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	key = "pfk_" + hex.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(key))
	_, err = s.Pool.Exec(ctx, `INSERT INTO merchant_api_keys(id,merchant_id,key_hash) VALUES ($1,$2,$3)`, id, merchant, digest[:])
	if err != nil {
		return "", "", err
	}
	return id, key, nil
}

func (s *Store) Authenticate(ctx context.Context, key string) (string, error) {
	if len(key) != 68 || !strings.HasPrefix(key, "pfk_") {
		return "", ErrUnauthenticated
	}
	if _, err := hex.DecodeString(key[4:]); err != nil {
		return "", ErrUnauthenticated
	}
	digest := sha256.Sum256([]byte(key))
	var merchant string
	err := s.Pool.QueryRow(ctx, `SELECT merchant_id FROM merchant_api_keys WHERE key_hash=$1 AND revoked_at IS NULL`, digest[:]).Scan(&merchant)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthenticated
	}
	return merchant, err
}

func (s *Store) Revoke(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE merchant_api_keys SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("active credential not found")
	}
	return nil
}

// Rotate issues a successor before revoking the old credential. Both changes
// are committed together so a failed rotation leaves the original usable.
func (s *Store) Rotate(ctx context.Context, id string) (newID, key string, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)
	var merchant string
	err = tx.QueryRow(ctx, `SELECT merchant_id FROM merchant_api_keys WHERE id=$1 AND revoked_at IS NULL FOR UPDATE`, id).Scan(&merchant)
	if err != nil {
		return "", "", err
	}
	var secret, rawID [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return "", "", err
	}
	if _, err = rand.Read(rawID[:16]); err != nil {
		return "", "", err
	}
	rawID[6] = rawID[6]&0x0f | 0x40
	rawID[8] = rawID[8]&0x3f | 0x80
	h := hex.EncodeToString(rawID[:16])
	newID = h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	key = "pfk_" + hex.EncodeToString(secret[:])
	digest := sha256.Sum256([]byte(key))
	if _, err = tx.Exec(ctx, `INSERT INTO merchant_api_keys(id,merchant_id,key_hash) VALUES ($1,$2,$3)`, newID, merchant, digest[:]); err != nil {
		return "", "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE merchant_api_keys SET revoked_at=now() WHERE id=$1`, id); err != nil {
		return "", "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", "", err
	}
	return newID, key, nil
}
