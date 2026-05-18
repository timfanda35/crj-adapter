# crj-adapter

A small Cloud Run **service** that receives Pub/Sub push deliveries and starts a Cloud Run **job** execution, forwarding the message payload to the job container via per-execution overrides.

```
Cloud Scheduler → Pub/Sub topic → Push subscription → crj-adapter (Cloud Run service) → Cloud Run job
```

## Why

Cloud Run jobs don't accept HTTP, and Pub/Sub push subscriptions only deliver to HTTP endpoints. This adapter bridges the gap. One deployment can dispatch to many jobs based on a Pub/Sub message attribute.

## Message contract

The publisher (typically Cloud Scheduler) sets attributes on every message:

| Attribute    | Required | Meaning                                                                 |
|--------------|----------|-------------------------------------------------------------------------|
| `job_name`   | yes      | Target Cloud Run job name. Without it the message is dropped.           |
| `job_region` | no       | Region of the target job. Falls back to the adapter's `DEFAULT_REGION`. |
| `job_args`   | no       | JSON array of strings — replaces the job container's `args` if present. |

The adapter forwards the message to the job container as environment variables:

| Env var               | Value                                                         |
|-----------------------|---------------------------------------------------------------|
| `PUBSUB_MESSAGE_ID`   | Pub/Sub message ID                                            |
| `PUBSUB_PUBLISH_TIME` | RFC3339 publish timestamp                                     |
| `PUBSUB_DATA`         | Message body as **raw base64** (the job decodes when it knows the format) |
| `PUBSUB_ATTR_<KEY>`   | One per attribute, key uppercased                              |

## Deploy

```sh
export PROJECT=my-project
export REGION=asia-east1

# 1. Service account for the adapter
gcloud iam service-accounts create crj-adapter-sa --project $PROJECT

# 2. Grant invoker on the target jobs (run.invoker includes run.jobs.run)
gcloud run jobs add-iam-policy-binding daily-report \
  --region $REGION \
  --member "serviceAccount:crj-adapter-sa@$PROJECT.iam.gserviceaccount.com" \
  --role roles/run.invoker

# 3. Deploy the adapter
gcloud run deploy crj-adapter \
  --source . \
  --region $REGION \
  --service-account crj-adapter-sa@$PROJECT.iam.gserviceaccount.com \
  --set-env-vars PROJECT_ID=$PROJECT,DEFAULT_REGION=$REGION \
  --no-allow-unauthenticated

# 4. Service account that Pub/Sub uses to call the adapter
gcloud iam service-accounts create pubsub-invoker-sa --project $PROJECT
gcloud run services add-iam-policy-binding crj-adapter \
  --region $REGION \
  --member "serviceAccount:pubsub-invoker-sa@$PROJECT.iam.gserviceaccount.com" \
  --role roles/run.invoker

# 5. Create the subscription
ADAPTER_URL=$(gcloud run services describe crj-adapter --region $REGION --format='value(status.url)')
gcloud pubsub subscriptions create dispatch-sub \
  --topic job-dispatch \
  --push-endpoint "$ADAPTER_URL/" \
  --push-auth-service-account pubsub-invoker-sa@$PROJECT.iam.gserviceaccount.com
```

## Trigger with Cloud Scheduler

```sh
gcloud scheduler jobs create pubsub run-daily-report \
  --schedule "0 3 * * *" \
  --time-zone "Asia/Taipei" \
  --topic projects/$PROJECT/topics/job-dispatch \
  --message-body '{"report_date":"2026-05-19","tenant_id":"acme"}' \
  --attributes "job_name=daily-report,job_region=$REGION"
```

The job container sees `PUBSUB_DATA=eyJyZXBvcnRfZGF0ZSI6...`, decodes it, and runs.

### Passing CLI args instead

If your job's entrypoint takes CLI args, use the `job_args` attribute:

```sh
--attributes 'job_name=daily-report,job_args=["--date=2026-05-19","--tenant=acme"]'
```

## Local development

```sh
go test ./...
PROJECT_ID=test DEFAULT_REGION=us-central1 go run .

# In another shell:
curl -X POST localhost:8080/ -H 'content-type: application/json' -d '{
  "subscription":"projects/p/subscriptions/s",
  "message":{
    "messageId":"1",
    "publishTime":"2026-01-01T00:00:00Z",
    "data":"aGVsbG8=",
    "attributes":{"job_name":"echo-job"}
  }
}'
```

(With real credentials in the environment the adapter will attempt a real `RunJob` call.)

## Error semantics

| Situation                         | HTTP   | Pub/Sub behavior |
|-----------------------------------|--------|------------------|
| Malformed JSON envelope           | 400    | Ack (no retry)   |
| Missing `job_name` attribute      | 204    | Ack (no retry)   |
| Malformed `job_args` attribute    | 204    | Ack (no retry)   |
| `NotFound` / `PermissionDenied` / `InvalidArgument` / `FailedPrecondition` / `Unauthenticated` from `RunJob` | 204 | Ack (no retry) |
| Other `RunJob` failure (network, 5xx, throttling) | 503 | Retry with backoff |
| Successfully kicked off          | 204    | Ack              |

Configure a dead-letter topic on the subscription to capture poison-pill messages.

## What's out of scope

- Waiting for the job to finish or reporting its result back.
- Per-message `taskCount` / `timeout` overrides.
- Writing large payloads to GCS (Pub/Sub's 10 MB message limit is plenty for env overrides).
