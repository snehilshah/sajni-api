# Deploying sajni-api

Backend ships to **Google Cloud Run** (region `asia-south1`, Mumbai)
on `sga/release/v*` git tags. Cloud Run is the cheapest path here:
scale-to-zero means you pay $0 when nobody's using the app, and the
free tier (2M requests/month + 360k vCPU-sec + 180k GB-sec) covers
hobby usage entirely.

> **A note on the region.** You said `asia-south1-c` — that's a
> *zone*, used only for Compute Engine VMs. Cloud Run, Artifact
> Registry, and GCS all use *regions*. The right value here is
> `asia-south1` (the Mumbai region). The workflow uses that.

```
push to main           ─►  CI: gofmt · vet · build · test
push tag sga/release/v*─►  CI gate → docker build → Artifact Registry
                            ─►  Cloud Run deploy + /readyz smoke test
```

The frontend is a separate repo (`ohmysajni/sajni-web`) — it deploys
to Vercel and calls this service through same-origin `/api/*` rewrites
to the Cloud Run default URL.

---

## Cost math (worst case for a hobby app)

| Component                 | Tier / config                           | Approx cost  |
| ------------------------- | --------------------------------------- | ------------ |
| Cloud Run                 | min=0, max=2, cpu=1, mem=512Mi          | **$0** under free tier |
| Artifact Registry         | <500MB images, lifecycle keeps 5 latest | <$0.10/mo |
| Secret Manager            | 4 secrets, ≤6 free                      | $0          |
| GCS (`sajni-blobs`)       | Standard, single region, <1GB           | <$0.05/mo |
| Cloud Tasks               | First 1M ops/month free                 | $0          |
| Cloud Run domain mapping  | `api.ohmysajni.com`                     | $0          |
| **Postgres**              | **Use Supabase / Neon free tier**       | $0          |
|                           | (Cloud SQL min ≈ ₹830/mo — avoid)      |              |
| Egress to Vercel          | First 1GB/mo free                       | $0          |

Net: ~₹0/month while you're under the free tier. Set up a budget
alert in Billing for ~₹100/mo so any drift pings you.

---

## One-time setup

```sh
PROJECT_ID=ohmysajni
REGION=asia-south1
GH_REPO=ohmysajni/sajni-api

gcloud config set project "$PROJECT_ID"

gcloud services enable \
  run.googleapis.com \
  artifactregistry.googleapis.com \
  secretmanager.googleapis.com \
  iamcredentials.googleapis.com \
  cloudtasks.googleapis.com \
  storage.googleapis.com

# Container registry. Add a cleanup policy to cap storage.
gcloud artifacts repositories create sajni \
  --location="$REGION" --repository-format=docker

cat > /tmp/keep-recent.json <<'EOF'
[{
  "name": "keep-5-most-recent",
  "action": {"type": "Keep"},
  "mostRecentVersions": {"keepCount": 5}
}, {
  "name": "delete-older-than-30d",
  "action": {"type": "Delete"},
  "condition": {"olderThan": "2592000s"}
}]
EOF
gcloud artifacts repositories set-cleanup-policies sajni \
  --location="$REGION" --policy=/tmp/keep-recent.json

# GCS for note/journal blobs (Cloud Run filesystem is ephemeral).
gcloud storage buckets create "gs://sajni-blobs" \
  --location="$REGION" --uniform-bucket-level-access

# Cloud Tasks queue for exact reminder delivery. The app enqueues reminders
# here and Cloud Tasks calls POST /internal/reminders/fire at the right time.
gcloud tasks queues create sajni-reminders --location="$REGION"
```

### Secrets

> **Why Secret Manager and not GitHub Actions secrets?** GitHub
> Actions secrets are only available *during* the workflow run — the
> built container never sees them. Cloud Run reads its runtime env
> from either plain values in the service spec (`env_vars:`) or from
> GCP Secret Manager references (`secrets:`). The workflow stitches
> the two together at deploy time. Anything sensitive (API keys, DB
> URL, JWT key, OAuth client secrets, the OAuth client *ids* by
> convention) lives in Secret Manager. Anything non-sensitive (URLs,
> bucket names, model names) lives in `env_vars:` and is sourced from
> GitHub repo Variables.

All runtime config — including URL-ish things that aren't strictly
sensitive — lives in Secret Manager so there's a single source of
truth. The secret name on the left becomes the `secrets:` reference
in `.github/workflows/deploy.yml`, which Cloud Run mounts as the
matching env var at boot.

```sh
echo -n "postgres://USER:PASS@HOST:5432/sajni?sslmode=require" \
  | gcloud secrets create sajni-database-url --data-file=-

openssl rand -hex 32 \
  | gcloud secrets create sajni-jwt-secret --data-file=-

# Optional integrations. Create the secret even if empty — the
# deploy workflow references all of these unconditionally.
echo -n "your-tmdb-key"   | gcloud secrets create sajni-tmdb-key   --data-file=-
echo -n "your-gemini-key" | gcloud secrets create sajni-gemini-key --data-file=-

# Auth: OAuth client credentials + Resend API key (added with the
# auth rework). Names use the same SCREAMING_SNAKE_CASE that the env
# var has — easier to grep, no `sajni-` prefix needed.
echo -n "GOOGLE_CLIENT_ID"      | gcloud secrets create GOOGLE_OAUTH_CLIENT_ID     --data-file=-
echo -n "GOOGLE_CLIENT_SECRET"  | gcloud secrets create GOOGLE_OAUTH_CLIENT_SECRET --data-file=-
echo -n "GITHUB_CLIENT_ID"      | gcloud secrets create GITHUB_OAUTH_CLIENT_ID     --data-file=-
echo -n "GITHUB_CLIENT_SECRET"  | gcloud secrets create GITHUB_OAUTH_CLIENT_SECRET --data-file=-
echo -n "re_xxxxx_resendkey"    | gcloud secrets create RESEND_API_KEY             --data-file=-

# Plain runtime config promoted into Secret Manager for consistency.
echo -n "https://www.ohmysajni.com"    | gcloud secrets create APP_URL        --data-file=-
echo -n "https://www.ohmysajni.com"    | gcloud secrets create CORS_ORIGIN    --data-file=-
echo -n "https://sajni-api-REGION-HASH.a.run.app" | gcloud secrets create API_BASE_URL --data-file=-
echo -n "Sajni <hello@ohmysajni.com>"  | gcloud secrets create EMAIL_FROM     --data-file=-
echo -n "info"                         | gcloud secrets create LOG_LEVEL      --data-file=-
openssl rand -hex 32                   | gcloud secrets create REMINDER_CRON_SECRET --data-file=-
```

For OAuth providers, register the same-origin callback URLs that hit
Vercel first:

```text
https://www.ohmysajni.com/api/auth/google/callback
https://www.ohmysajni.com/api/auth/github/callback
```

Keep the Cloud Run callback URLs registered only as fallback/debug
URLs. The same-origin callback matters: the refresh cookie must be set
on the frontend host so `/api/auth/refresh` still has it after browser
reload.

Use the canonical frontend host here. Current DNS redirects
`ohmysajni.com` to `www.ohmysajni.com`, so `www.ohmysajni.com` is the
host that should appear in `APP_URL` and OAuth callback URLs.

Rotate later with `gcloud secrets versions add NAME --data-file=-`.
The deploy uses `:latest` so a fresh revision picks up rotations.

To rotate a key without rebuilding the image:

```sh
echo -n "NEW_VALUE" | gcloud secrets versions add sajni-google-oauth-client-id --data-file=-
# Roll the service so it picks up the new version. No image rebuild.
gcloud run services update sajni-api --region asia-south1
```

### Service account + Workload Identity Federation

```sh
SA=sajni-deployer
gcloud iam service-accounts create "$SA" \
  --display-name="sajni-api deployer (GitHub Actions)"
SA_EMAIL="${SA}@${PROJECT_ID}.iam.gserviceaccount.com"

for role in \
  roles/run.admin \
  roles/iam.serviceAccountUser \
  roles/artifactregistry.writer \
  roles/secretmanager.secretAccessor \
  roles/storage.objectAdmin
do
  gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member="serviceAccount:${SA_EMAIL}" --role="$role"
done

gcloud iam workload-identity-pools create github \
  --location=global --display-name="GitHub Actions"

gcloud iam workload-identity-pools providers create-oidc github-provider \
  --location=global --workload-identity-pool=github \
  --display-name="GitHub Actions Provider" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository" \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-condition="assertion.repository == '${GH_REPO}'"

PROJECT_NUMBER=$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')

gcloud iam service-accounts add-iam-policy-binding "$SA_EMAIL" \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github/attribute.repository/${GH_REPO}"

# Note this — it's the GCP_WIF_PROVIDER value below.
echo "projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/github/providers/github-provider"

# Let the Cloud Run runtime identity enqueue reminder tasks. The workflow does
# not set a custom runtime service account, so Cloud Run uses the Compute
# Engine default service account.
RUNTIME_SA="${PROJECT_NUMBER}-compute@developer.gserviceaccount.com"
gcloud projects add-iam-policy-binding "$PROJECT_ID" \
  --member="serviceAccount:${RUNTIME_SA}" \
  --role=roles/cloudtasks.enqueuer
```

### GitHub configuration

In **Settings → Secrets and variables → Actions** of `ohmysajni/sajni-api`,
add these as **Variables** (not Secrets — they're identifiers, not credentials):

GitHub repo Variables hold the values the *workflow itself* needs to
deploy — they're never seen by the running container, and they aren't
sensitive (project ids, region names, the WIF provider path). Runtime
config lives in Secret Manager (above).

| Variable              | Value                                                                                            |
| --------------------- | ------------------------------------------------------------------------------------------------ |
| `GCP_PROJECT_ID`      | `ohmysajni`                                                                                      |
| `GCP_REGION`          | `asia-south1`                                                                                    |
| `GCP_SERVICE_ACCOUNT` | `sajni-deployer@ohmysajni.iam.gserviceaccount.com`                                               |
| `GCP_WIF_PROVIDER`    | the line you `echo`d at the end of the IAM block                                                 |
| `GCS_BUCKET`          | e.g. `ohmysajni-sajni-blobs`                                                                     |

---

## Scheduler cleanup

Reminders are delivered by Cloud Tasks now. Keep the old reminder
Cloud Scheduler job only as a safety sweep, and slow it down from
`*/5` to once daily:

```sh
gcloud scheduler jobs list --location="$REGION"
gcloud scheduler jobs update http REMINDER_JOB_NAME \
  --location="$REGION" \
  --schedule="0 3 * * *" \
  --time-zone="Asia/Kolkata"
```

Trading (and with it stock/ETF price fetching) was removed in 2026-07.
Delete the old price scheduler job and its secret after this revision
is deployed:

```sh
gcloud scheduler jobs delete PRICE_JOB_NAME --location="$REGION"
gcloud secrets delete PRICE_CRON_SECRET
```

After a month of clean reminder logs, you can delete the daily reminder
sweep too. Until then it catches missed Cloud Tasks deliveries with the
same idempotency gates.

## Weekly / monthly task digests

Week and month tasks have no `scheduled_at`, so the scheduled-time reminder
never fires for them. A once-daily sweep at **10:00 IST** posts to
`/internal/reminders/digest` (same `REMINDER_CRON_SECRET` header). The
endpoint self-gates: it emails the pending **week** tasks only on Fridays
and the pending **month** tasks only on the last calendar day of the month,
so a single daily job covers both. Create it once:

```sh
gcloud scheduler jobs create http sajni-digest \
  --location="$REGION" \
  --schedule="0 10 * * *" \
  --time-zone="Asia/Kolkata" \
  --uri="$API_BASE_URL/internal/reminders/digest" \
  --http-method=POST \
  --headers="X-Reminder-Cron=$(gcloud secrets versions access latest --secret=REMINDER_CRON_SECRET)"
```

Idempotent via `tasks.digested_at`: a still-pending task is re-nudged each
cycle, and a task added after a fire is caught on the next one. Safe to run
any day — it no-ops when neither a Friday nor a month-end applies.

---

## Releasing

```sh
# Make sure CI is green on main, then:
git tag sga/release/v0.1.0
git push origin sga/release/v0.1.0
```

The workflow:

1. Re-runs gofmt-check + vet + build + test.
2. Builds the Dockerfile here, pushes to
   `…/sajni-api:v0.1.0` and `…/sajni-api:latest` in Artifact Registry.
3. Deploys to Cloud Run with the cheapest sane flags.
4. Hits `/readyz` on the new revision; fails the workflow if it
   doesn't come up in ~25s.

### Rollback

```sh
# Re-deploy a previous image tag in <30s.
gcloud run deploy sajni-api --region asia-south1 \
  --image=asia-south1-docker.pkg.dev/ohmysajni/sajni-api/sajni-api:v0.0.9
```

Or instantly route 100% of traffic to a previous revision:

```sh
gcloud run services update-traffic sajni-api --region asia-south1 \
  --to-revisions=PREVIOUS_REV=100
```

---

## Local dev

```sh
make dev        # go run ./cmd against your local Postgres
make check      # what CI runs (gofmt-check + vet + build + test)
make docker-run # build the Cloud Run image and run with .env
```

`.env.example` shows the variables; copy to `.env` and fill in
`DATABASE_URL`, `JWT_SECRET`, optional `TMDB_API_KEY` / `GEMINI_API_KEY`.

---

## Why blobs go to GCS, not the filesystem

`STORAGE_BACKEND=local` writes journal/note/upload bodies under
`./data/blobs/`. That's fine on your laptop but **wrong** on Cloud Run:

1. Cloud Run's filesystem is in-memory and **per-instance**. Two
   instances see two unrelated copies; a new revision wipes both.
2. Cold-starting a container loses any local state.

The deploy workflow sets `STORAGE_BACKEND=gcs` + `GCS_BUCKET`, and the
deploy SA has `roles/storage.objectAdmin`, so reads/writes Just Work.
Migrate any local dev blobs once before going to prod:

```sh
gcloud storage cp -r data/blobs/* gs://sajni-blobs/blobs/
```

---

## Useful one-offs

```sh
# What's running right now?
gcloud run services describe sajni-api --region asia-south1 \
  --format='value(status.latestReadyRevisionName, status.url)'

# Tail logs.
gcloud run services logs read sajni-api --region asia-south1 --limit=50

# Bump a runtime knob without rebuilding.
gcloud run services update sajni-api --region asia-south1 --memory=1Gi

# Add/override an env var (next deploy re-overrides).
gcloud run services update sajni-api --region asia-south1 \
  --update-env-vars=NEW_FLAG=value
```
