package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// ConfigureOpenTelemetry installs a local SDK meter provider. PAYFLOW records
// the same bounded instruments through OpenTelemetry and its local Prometheus
// endpoint. A deployment may replace the reader with an OTLP exporter later.
func ConfigureOpenTelemetry() func(context.Context) error {
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	otel.SetMeterProvider(provider)
	return provider.Shutdown
}
