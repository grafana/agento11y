module github.com/grafana/agento11y/examples/getting-started/go

go 1.25.7

require (
	github.com/grafana/agento11y/go v0.2.0
	github.com/joho/godotenv v1.5.1
	github.com/openai/openai-go/v3 v3.29.0
	go.opentelemetry.io/contrib/exporters/autoexport v0.60.0
	go.opentelemetry.io/otel v1.44.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/sdk/metric v1.43.0
	go.opentelemetry.io/otel/semconv v1.26.0
)

replace github.com/grafana/agento11y/go => ../../../go
