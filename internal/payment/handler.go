package payment

import (
	"context"
	"errors"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
	paymentv1 "payflow/gen/go/payflow/v1"
)

// Handler adapts the domain service to the generated Connect service.
type Handler struct{ Service *Service }

func (h *Handler) CreatePayment(ctx context.Context, req *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "request is required")
	}
	p, err := h.Service.Create(ctx, CreateInput{
		MerchantID: req.GetMerchantId(), IdempotencyKey: req.GetIdempotencyKey(),
		Amount: req.GetAmount(), Currency: req.GetCurrency(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &paymentv1.CreatePaymentResponse{Payment: toProto(p)}, nil
}

func (h *Handler) GetPayment(ctx context.Context, req *paymentv1.GetPaymentRequest) (*paymentv1.GetPaymentResponse, error) {
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "request is required")
	}
	p, err := h.Service.Get(ctx, req.GetPaymentId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &paymentv1.GetPaymentResponse{Payment: toProto(p)}, nil
}

func toProto(p Payment) *paymentv1.Payment {
	return &paymentv1.Payment{
		PaymentId: p.ID, MerchantId: p.MerchantID, Amount: p.Amount,
		Currency: p.Currency, Status: paymentv1.PaymentStatus_PAYMENT_STATUS_CREATED,
		CreatedAt: timestamppb.New(p.CreatedAt), UpdatedAt: timestamppb.New(p.UpdatedAt),
	}
}

func rpcError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, "merchant_id, idempotency_key, a positive amount, and JPY currency are required").WithCause(err)
	case errors.Is(err, ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, "idempotency key is already used for a different payload").WithCause(err)
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "payment not found").WithCause(err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, "request canceled").WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, "request deadline exceeded").WithCause(err)
	default:
		return connect.NewError(connect.CodeInternal, "payment operation failed").WithCause(err)
	}
}
