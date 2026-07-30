# Stacktrail Go SDK

Distributed tracing for asynchronous Go workloads.

## Contents

[Installation](#installation) | [Quick start](#quick-start) | [Configuration](#configuration) | [Job tracking](#job-tracking) | [Context propagation](#context-propagation) | [Architecture](#architecture) | [More examples](#more-examples)

## Installation

```bash
go get github.com/stacktraildev/stacktrail-go
```

## Quick start

Create a `.env` file:

```dotenv
STACKTRAIL_API_KEY=st_your_api_key
STACKTRAIL_SERVICE_NAME=report-worker
STACKTRAIL_ENV=development

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
| `STACKTRAIL_ENV` | Yes | — | Must be `development`, `staging`, or `production`. Powers the Jobs environment filter. |
| `STACKTRAIL_SERVICE_NAME` | No | Executable name | Service name attached to telemetry. |
| `STACKTRAIL_TRANSPORT` | No | `http` | Selects the OTLP/HTTP transport. Stacktrail sends telemetry over HTTPS by default. Use `grpc` for an OTLP collector. |
| `STACKTRAIL_COLLECTOR_ENDPOINT` | No | Hosted HTTPS endpoint or `localhost:4317` for gRPC | A custom HTTP(S) URL or host and port for OTLP/HTTP; a host and port for gRPC. |
| `STACKTRAIL_SECURE` | No | `true` | TLS setting. It must match an explicit HTTP URL scheme: `https://` requires `true`; `http://` requires `false`. |

### Transport behavior

| Use case | `STACKTRAIL_TRANSPORT` | `STACKTRAIL_COLLECTOR_ENDPOINT` | `STACKTRAIL_SECURE` | Result |
| --- | --- | --- | --- | --- |
| Stacktrail hosted ingestion (HTTPS by default) | Omit or `http` | Omit | Omit or `true` | Sends telemetry directly to Stacktrail over HTTPS. Start beacons are available only when using the hosted service. |
| Private HTTPS ingestion | `http` | `https://collector.example.com` | `true` | Sends telemetry to the custom HTTPS endpoint. |
| Local HTTP ingestion | `http` | `http://127.0.0.1:4318` | `false` | Sends telemetry to a local or private HTTP endpoint. |
| TLS gRPC collector | `grpc` | Collector host and port | `true` | Sends telemetry to the collector over TLS. |
| Local gRPC collector | `grpc` | `localhost:4317` or another local host and port | `false` | Sends telemetry to the collector without TLS. |

Use a complete HTTP(S) URL when the collector has a non-default trace path. Otherwise, the SDK appends `/v1/traces`. A bare HTTP host and port derives its scheme from `STACKTRAIL_SECURE`. Custom HTTP(S) endpoints and all gRPC collectors do not send hosted start beacons.

For a local OTLP/HTTP collector:

```dotenv
STACKTRAIL_API_KEY=st_your_api_key
STACKTRAIL_SERVICE_NAME=report-worker
STACKTRAIL_ENV=development
STACKTRAIL_TRANSPORT=http
STACKTRAIL_COLLECTOR_ENDPOINT=http://127.0.0.1:4318
STACKTRAIL_SECURE=false
```

For a local gRPC collector:

```dotenv
STACKTRAIL_API_KEY=st_your_api_key
STACKTRAIL_SERVICE_NAME=report-worker
STACKTRAIL_ENV=development
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

### Context propagation

Pass a job context when work continues across an application-owned asynchronous
boundary outside direct nesting:

```go
parent := stacktrail.StartJob(ctx, "process-order")

followUp := stacktrail.StartJob(parent.Context(), "send-follow-up")
followUp.Success()
parent.Success()
```

Pass the parent context explicitly when it is needed.

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

For hosted HTTPS ingestion, `StartJob` also sends an asynchronous start signal. This allows Stacktrail to identify jobs that crash before their final trace is delivered without affecting application work.

### OTLP collector ingestion

```text
Application → Stacktrail SDK → OTLP Collector → Stacktrail Backend → Database
```

## More examples

Explore framework integrations and complete examples in the
[Stacktrail documentation](https://docs.stacktrail.dev/examples).
