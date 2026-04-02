# OTLP Log Export & Trace Correlation

This branch (`feat/otlp-log-export`) adds the ability to export OpenFGA logs via
OTLP and correlate them with distributed traces.

## What was changed

### 1. OTLP log export via otelzap bridge (`6606e1a7`)

Added a new otelzap bridge core that sends a copy of every zap log entry to an
OTLP collector as an OTel log record. When the `OPENFGA_LOG_OTLP_ENDPOINT` env
var is set, logs are exported via OTLP **in addition to** stdout.

Key files:

- `internal/telemetry/logging.go` — OTLP log provider setup
- `pkg/logger/logger.go` — `WithOTELCore` option, `contextFilterCore` wrapper
- `pkg/server/config/config.go` — `OTLPLogConfig` configuration
- `cmd/run/run.go` — wiring it all together

The stdout core is wrapped with a `contextFilterCore` that strips the `ctx`
field (only meaningful to the otelzap bridge). The two cores are combined with
`zapcore.NewTee`.

### 2. Pass context through gRPC logging interceptor (`deb453c4`)

Changed the gRPC logging interceptor to use `*WithContext` methods
(`InfoWithContext`, `ErrorWithContext`, `DebugWithContext`) instead of the plain
`Info`, `Error`, `Debug` methods.

The `*WithContext` methods append `zap.Any("ctx", ctx)` to the log fields. The
otelzap bridge extracts the span context from this `context.Context`, which
attaches `traceID` and `spanID` to the OTLP log record. Without this change, the
OTLP-exported logs had no trace correlation.

Key file: `pkg/middleware/logging/logging.go`

### 3. GCP stdout trace correlation fields (`1608656f`, debug)

Added `logging.googleapis.com/trace`, `logging.googleapis.com/spanId`, and
`logging.googleapis.com/trace_sampled` as JSON fields in the stdout log output.

Cloud Run's built-in log agent recognizes these field names and promotes them to
top-level Cloud Logging `LogEntry` fields, enabling log-trace correlation for
the stdout path (without OTLP).

This is currently hardcoded for GCP and should be made configurable before
merging upstream.

Key file: `pkg/middleware/logging/logging.go`

## How trace correlation works

There are two independent paths for trace-correlated logs:

```
                     gRPC request (with trace context)
                                  |
                                  v
                    OpenFGA gRPC logging interceptor
                    (extracts spanCtx from ctx)
                                  |
                   +--------------+--------------+
                   |                             |
                   v                             v
            stdout (JSON)                 otelzap bridge
            with logging.googleapis.com   (attaches spanCtx
            trace/spanId fields           to OTel log record)
                   |                             |
                   v                             v
         Cloud Run log agent              OTLP collector
         (promotes fields to              (googlecloud exporter
          LogEntry.trace/spanId)           maps to LogEntry)
                   |                             |
                   +-------------+---------------+
                                 |
                                 v
                          Cloud Logging
                    (logs correlated by trace)
```

## Configuration

New env vars:

| Variable                       | Description                                                        |
| ------------------------------ | ------------------------------------------------------------------ |
| `OPENFGA_LOG_OTLP_ENDPOINT`    | OTLP collector endpoint (e.g. `127.0.0.1:4317`). Empty = disabled. |
| `OPENFGA_LOG_OTLP_TLS_ENABLED` | Use TLS for OTLP connection. Default: `false`.                     |
| `GOOGLE_CLOUD_PROJECT`         | GCP project ID for stdout trace field formatting.                  |

## Infrastructure requirements (GCP)

> [!NOTE]
>
> `feat/custom-openfga-with-otlp-logs` branch

When using the OTLP path with the `googlecloud` exporter in the otel collector,
the following changes are needed in the deployment infrastructure:

### 1. OTel Collector config

Add a `logs` pipeline and set `default_log_name` on the `googlecloud` exporter:

```yaml
exporters:
  googlecloud:
    log:
      default_log_name: openfga # required, otherwise logs are rejected

service:
  pipelines:
    # existing traces pipeline ...
    logs:
      receivers: [otlp]
      processors: [resourcedetection]
      exporters: [googlecloud]
```

### 2. IAM

The service account running the OTel collector (or the OpenFGA service account,
if the collector runs as a sidecar) needs the log writer role:

```hcl
resource "google_project_iam_member" "openfga_log_writer" {
  project = data.google_project.self.project_id
  role    = "roles/logging.logWriter"
  member  = "serviceAccount:${google_service_account.openfga_service_account.email}"
}
```

### 3. Cloud Run / Kubernetes env vars

Set the OTLP endpoint on the OpenFGA container (pointing at the collector
sidecar):

```yaml
env:
  - name: OPENFGA_LOG_OTLP_ENDPOINT
    value: "127.0.0.1:4317"
```

If using the GCP stdout trace correlation path (commit `1608656f`), also set:

```yaml
env:
  - name: GOOGLE_CLOUD_PROJECT
    value: "<your-gcp-project-id>"
```
