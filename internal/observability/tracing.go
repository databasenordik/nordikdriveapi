package observability

import (
	"context"
	"errors"
	"os"

	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func Tracing(ctx context.Context) (func(context.Context) error, error) {
	options := []sdktrace.TracerProviderOption{sdktrace.WithResource(resource.NewWithAttributes("", attribute.String("service.name", os.Getenv("K_SERVICE")), attribute.String("service.version", os.Getenv("K_REVISION")))), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1)))}
	mode := os.Getenv("OTEL_EXPORTER")
	if mode == "gcp" {
		exporter, err := texporter.New()
		if err != nil {
			return nil, errors.New("trace exporter initialization failed")
		}
		options = append(options, sdktrace.WithBatcher(exporter))
	} else if mode != "" && mode != "none" {
		return nil, errors.New("OTEL_EXPORTER must be gcp or none")
	}
	provider := sdktrace.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}
