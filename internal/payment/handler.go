package payment

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
	paymentv1 "payflow/gen/go/payflow/v1"
	"payflow/internal/auth"
	"payflow/internal/banking"
	"payflow/internal/ledger"
)

// Handler adapts the domain service to the generated Connect service.
type Handler struct {
	Service      *Service
	Processor    *Processor
	Orchestrator interface {
		Request(context.Context, string, banking.Scenario) (Payment, error)
	}
	Submitter interface {
		Submit(context.Context, string) (Payment, error)
	}
	DevProcessingEnabled bool
	AuthRequired         bool
	Ledger               interface {
		Balance(context.Context, string, string, string) (ledger.Balance, error)
	}
}

func (h *Handler) SubmitPayment(ctx context.Context, req *paymentv1.SubmitPaymentRequest) (*paymentv1.SubmitPaymentResponse, error) {
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "request is required")
	}
	if err := h.ownedPayment(ctx, req.GetPaymentId()); err != nil {
		return nil, err
	}
	if h.Submitter == nil {
		return nil, connect.NewError(connect.CodeUnavailable, "payment submission unavailable")
	}
	p, err := h.Submitter.Submit(ctx, req.GetPaymentId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &paymentv1.SubmitPaymentResponse{Payment: toProto(p)}, nil
}

func (h *Handler) merchant(ctx context.Context) (string, error) {
	merchant, ok := auth.Merchant(ctx)
	if h.AuthRequired && !ok {
		return "", connect.NewError(connect.CodeUnauthenticated, "API credential required")
	}
	return merchant, nil
}

func (h *Handler) ownedPayment(ctx context.Context, id string) error {
	merchant, err := h.merchant(ctx)
	if err != nil {
		return err
	}
	if !h.AuthRequired {
		return nil
	}
	p, err := h.Service.Get(ctx, id)
	if err != nil {
		return rpcError(err)
	}
	if p.MerchantID != merchant {
		return connect.NewError(connect.CodeNotFound, "payment not found")
	}
	return nil
}

func (h *Handler) SimulatePayment(ctx context.Context, req *paymentv1.SimulatePaymentRequest) (*paymentv1.SimulatePaymentResponse, error) {
	if !h.DevProcessingEnabled || (h.Processor == nil && h.Orchestrator == nil) {
		return nil, connect.NewError(connect.CodePermissionDenied, "simulation is disabled")
	}
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "request is required")
	}
	if err := h.ownedPayment(ctx, req.GetPaymentId()); err != nil {
		return nil, err
	}
	scenario, ok := scenarioFromProto(req.GetScenario())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, "simulation scenario is required")
	}
	var p Payment
	var err error
	if h.Orchestrator != nil {
		p, err = h.Orchestrator.Request(ctx, req.GetPaymentId(), scenario)
	} else {
		p, err = h.Processor.Process(ctx, req.GetPaymentId(), scenario)
	}
	if err != nil {
		return nil, rpcError(err)
	}
	return &paymentv1.SimulatePaymentResponse{Payment: toProto(p)}, nil
}

func (h *Handler) ReconcilePayment(ctx context.Context, req *paymentv1.ReconcilePaymentRequest) (*paymentv1.ReconcilePaymentResponse, error) {
	if !h.DevProcessingEnabled || h.Processor == nil {
		return nil, connect.NewError(connect.CodePermissionDenied, "simulation is disabled")
	}
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "request is required")
	}
	if err := h.ownedPayment(ctx, req.GetPaymentId()); err != nil {
		return nil, err
	}
	p, err := h.Processor.Reconcile(ctx, req.GetPaymentId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &paymentv1.ReconcilePaymentResponse{Payment: toProto(p)}, nil
}

func scenarioFromProto(s paymentv1.SimulationScenario) (banking.Scenario, bool) {
	switch s {
	case paymentv1.SimulationScenario_SIMULATION_SCENARIO_APPROVE:
		return banking.Approve, true
	case paymentv1.SimulationScenario_SIMULATION_SCENARIO_DECLINE:
		return banking.Decline, true
	case paymentv1.SimulationScenario_SIMULATION_SCENARIO_TEMPORARY_FAILURE:
		return banking.TemporaryFailure, true
	case paymentv1.SimulationScenario_SIMULATION_SCENARIO_NETWORK_TIMEOUT:
		return banking.NetworkTimeout, true
	case paymentv1.SimulationScenario_SIMULATION_SCENARIO_LOST_APPROVAL_RESPONSE:
		return banking.LostApprovalResponse, true
	default:
		return "", false
	}
}

func (h *Handler) CreatePayment(ctx context.Context, req *paymentv1.CreatePaymentRequest) (*paymentv1.CreatePaymentResponse, error) {
	merchant, authErr := h.merchant(ctx)
	if authErr != nil {
		return nil, authErr
	}
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "request is required")
	}
	if h.AuthRequired {
		if req.GetMerchantId() != "" && req.GetMerchantId() != merchant {
			return nil, connect.NewError(connect.CodePermissionDenied, "merchant ID does not match credential")
		}
	} else {
		merchant = req.GetMerchantId()
	}
	p, err := h.Service.Create(ctx, CreateInput{
		MerchantID: merchant, IdempotencyKey: req.GetIdempotencyKey(),
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
	if err := h.ownedPayment(ctx, req.GetPaymentId()); err != nil {
		return nil, err
	}
	p, err := h.Service.Get(ctx, req.GetPaymentId())
	if err != nil {
		return nil, rpcError(err)
	}
	return &paymentv1.GetPaymentResponse{Payment: toProto(p)}, nil
}

func (h *Handler) GetMerchantBalance(ctx context.Context, req *paymentv1.GetMerchantBalanceRequest) (*paymentv1.GetMerchantBalanceResponse, error) {
	merchant, err := h.merchant(ctx)
	if err != nil {
		return nil, err
	}
	if !h.AuthRequired {
		return nil, connect.NewError(connect.CodePermissionDenied, "merchant balance requires authentication")
	}
	if req == nil || req.GetCurrency() != "JPY" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "JPY currency is required")
	}
	if h.Ledger == nil {
		return nil, connect.NewError(connect.CodeInternal, "ledger unavailable")
	}
	b, err := h.Ledger.Balance(ctx, "MERCHANT_PAYABLE", merchant, req.GetCurrency())
	if errors.Is(err, pgx.ErrNoRows) {
		return &paymentv1.GetMerchantBalanceResponse{Currency: req.GetCurrency()}, nil
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, "ledger lookup failed").WithCause(err)
	}
	return &paymentv1.GetMerchantBalanceResponse{Currency: b.Currency, Amount: b.Amount}, nil
}

func toProto(p Payment) *paymentv1.Payment {
	status := paymentv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED
	switch p.Status {
	case StatusCreated:
		status = paymentv1.PaymentStatus_PAYMENT_STATUS_CREATED
	case StatusProcessing:
		status = paymentv1.PaymentStatus_PAYMENT_STATUS_PROCESSING
	case StatusCompleted:
		status = paymentv1.PaymentStatus_PAYMENT_STATUS_COMPLETED
	case StatusFailed:
		status = paymentv1.PaymentStatus_PAYMENT_STATUS_FAILED
	case StatusReconciliationRequired:
		status = paymentv1.PaymentStatus_PAYMENT_STATUS_RECONCILIATION_REQUIRED
	}
	return &paymentv1.Payment{
		PaymentId: p.ID, MerchantId: p.MerchantID, Amount: p.Amount,
		Currency: p.Currency, Status: status, ProviderOperationId: p.ProviderOperationID,
		CreatedAt: timestamppb.New(p.CreatedAt), UpdatedAt: timestamppb.New(p.UpdatedAt),
		ProcessingStartedAt: optionalTime(p.ProcessingStartedAt),
		CompletedAt:         optionalTime(p.CompletedAt), FailedAt: optionalTime(p.FailedAt),
		ReconciliationRequiredAt: optionalTime(p.ReconciliationRequiredAt),
	}
}

func optionalTime(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func rpcError(err error) error {
	switch {
	case errors.Is(err, ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, "invalid payment request").WithCause(err)
	case errors.Is(err, ErrConflict):
		return connect.NewError(connect.CodeAlreadyExists, "idempotency key is already used for a different payload").WithCause(err)
	case errors.Is(err, ErrScenarioConflict):
		return connect.NewError(connect.CodeAlreadyExists, "payment already uses a different simulation scenario").WithCause(err)
	case errors.Is(err, ErrInvalidTransition):
		return connect.NewError(connect.CodeFailedPrecondition, "payment cannot make this transition").WithCause(err)
	case errors.Is(err, ErrNotFound):
		return connect.NewError(connect.CodeNotFound, "payment not found").WithCause(err)
	case errors.Is(err, ErrWorkflowUnavailable):
		return connect.NewError(connect.CodeUnavailable, "workflow dispatch is queued for recovery").WithCause(err)
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, "request canceled").WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, "request deadline exceeded").WithCause(err)
	default:
		return connect.NewError(connect.CodeInternal, "payment operation failed").WithCause(err)
	}
}
