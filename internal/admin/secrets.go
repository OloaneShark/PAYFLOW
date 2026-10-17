package admin

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

type SecretsManagerWriter struct{ Client *secretsmanager.Client }

func (w *SecretsManagerWriter) Create(ctx context.Context, name, value string) (string, error) {
	if w == nil || w.Client == nil || name == "" || value == "" {
		return "", errors.New("secret delivery is not configured")
	}
	result, err := w.Client.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String(name), SecretString: aws.String(value),
		Description: aws.String("PAYFLOW simulated merchant API key; retrieve through the approved credential recipient role"),
	})
	if err != nil {
		return "", errors.New("secret delivery failed")
	}
	if result.ARN == nil {
		return "", errors.New("secret delivery returned no ARN")
	}
	return *result.ARN, nil
}
