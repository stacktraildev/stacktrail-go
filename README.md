# Stacktrail Go SDK

Distributed tracing for asynchronous Go workloads.

## Contents

[Installation](#installation) | [Quick start](#quick-start) | [Configuration](#configuration) | [Job tracking](#job-tracking) | [Architecture](#architecture) | [More examples](#more-examples)

## Installation

```bash
go get github.com/stacktraildev/stacktrail-go
```

## Quick start

Create a `.env` file:

```dotenv
STACKTRAIL_API_KEY=st_your_api_key
STACKTRAIL_SERVICE_NAME=report-worker

# Optional. These are the hosted-ingestion defaults.
STACKTRAIL_TRANSPORT=http
STACKTRAIL_SECURE=true
```

Initialize Stacktrail once during application startup, then wrap the work you
want to track:

```go
package main

import (
    "context"
    "log"

    "github.com/stacktraildev/stacktrail-go"
)

func main() {
    ctx := context.Background()

    if err := stacktrail.LoadDotEnv(".env"); err != nil {
        log.Fatal(err)
    }
    if err := stacktrail.Init(ctx); err != nil {
        log.Fatal(err)
    }
    defer func() { _ = stacktrail.Shutdown(ctx) }()

    job := stacktrail.StartJob(ctx, "generate-report")
    job.AddMetadata("report.type", "weekly")

    if err := generateReport(); err != nil {
        job.Fail(err)
        return
    }
    job.Success()
}
```

## Configuration

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `STACKTRAIL_API_KEY` | Yes | — | Authenticates ingestion and identifies the Stacktrail project. |
| `STACKTRAIL_SERVICE_NAME` | No | Executable name | Service name attached to telemetry. |
| `STACKTRAIL_TRANSPORT` | No | `http` | Use `grpc` only when sending through an OTLP collector. |
| `STACKTRAIL_COLLECTOR_ENDPOINT` | No | `localhost:4317` | OTLP collector host and port. Requires `STACKTRAIL_TRANSPORT=grpc`. |
| `STACKTRAIL_SECURE` | No | `true` | Set `false` only for a local collector without TLS. Requires `STACKTRAIL_TRANSPORT=grpc`. |

### Transport behavior

| Use case | `STACKTRAIL_TRANSPORT` | `STACKTRAIL_COLLECTOR_ENDPOINT` | `STACKTRAIL_SECURE` | Result |
| --- | --- | --- | --- | --- |
| Stacktrail hosted ingestion | Omit or `http` | Omit | Omit or `true` | The SDK sends telemetry directly to Stacktrail over HTTPS. |
| Private collector with TLS | `grpc` | Collector host and port | `true` | The SDK sends telemetry to the collector over TLS. |
| Local collector | `grpc` | `localhost:4317` or another local host and port | `false` | The SDK sends telemetry to the collector without TLS. |
| Invalid hosted setup | `http` | Any value | `false` | Initialization fails. Direct hosted ingestion does not allow a custom endpoint or disabled TLS. |

For a local OTLP collector:

```dotenv
STACKTRAIL_API_KEY=st_your_api_key
STACKTRAIL_SERVICE_NAME=report-worker
STACKTRAIL_TRANSPORT=grpc
STACKTRAIL_COLLECTOR_ENDPOINT=localhost:4317
STACKTRAIL_SECURE=false
```

## Job tracking

Use `StartJob` at the boundary of each background task. Complete it exactly once
with `Success`, `Fail`, or `End`.

```go
job := stacktrail.StartJob(ctx, "process-payment")
job.AddMetadata("payment.provider", "stripe")

if err := processPayment(); err != nil {
    job.Fail(err)
    return err
}
job.Success()
return nil
```

`job.End(nil)` records success and `job.End(err)` records failure. Completion is
idempotent and safe to invoke concurrently.

### Nested jobs

```go
parent := stacktrail.StartJob(ctx, "process-order")

payment := parent.StartChildJob("charge-payment")
payment.Success()

email := parent.StartChildJob("send-confirmation")
email.Success()

parent.Success()
```

### Events

```go
job := stacktrail.StartJob(ctx, "data-import")
job.AddEvent("validation-started")
job.AddEvent("import-started", attribute.Int("record_count", 1000))
job.Success()
```

Use `stacktrail.ForceFlush(ctx)` before a short-lived process exits when you
need telemetry exported before shutdown.

## Architecture

### Hosted ingestion

```text
Application → Stacktrail SDK → Stacktrail Backend → Database
```

### OTLP collector ingestion

```text
Application → Stacktrail SDK → OTLP Collector → Stacktrail Backend → Database
```

## More examples

Explore framework integrations and complete examples in the
[Stacktrail documentation](https://docs.stacktrail.dev/examples).
