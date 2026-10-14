// Package banking implements a durable, local-only simulated banking provider.
package banking

import (
	"context"
	"errors"
)

type Scenario string

const (
	Approve              Scenario = "APPROVE"
	Decline              Scenario = "DECLINE"
	TemporaryFailure     Scenario = "TEMPORARY_FAILURE"
	NetworkTimeout       Scenario = "NETWORK_TIMEOUT"
	LostApprovalResponse Scenario = "LOST_APPROVAL_RESPONSE"
)

func (s Scenario) Valid() bool {
	switch s {
	case Approve, Decline, TemporaryFailure, NetworkTimeout, LostApprovalResponse:
		return true
	default:
		return false
	}
}

type Outcome string

const (
	Unknown  Outcome = "PENDING"
	Approved Outcome = "APPROVED"
	Declined Outcome = "DECLINED"
)

var (
	ErrTemporary    = errors.New("simulated provider temporary failure")
	ErrTimeout      = errors.New("simulated provider network timeout")
	ErrLostResponse = errors.New("simulated provider approval response lost")
)

type Operation struct {
	ID        string
	PaymentID string
	Scenario  Scenario
}

type Provider interface {
	Execute(context.Context, Operation) (Outcome, error)
	Lookup(context.Context, Operation) (Outcome, error)
}
