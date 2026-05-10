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
                            ─►  Cloud Run deploy + /healthz smoke test
```

The frontend is a separate repo (`ohmysajni/sajni-web`) — it deploys
to Vercel and calls this service over HTTPS at `api.ohmysajni.com`.

---

## Cost math (worst case for a hobby app)

| Component                 | Tier / config                           | Approx cost  |
| ------------------------- | --------------------------------------- | ------------ |
| Cloud Run                 | min=0, max=2, cpu=1, mem=512Mi          | **$0** under free tier |
| Artifact Registry         | <500MB images, lifecycle keeps 5 latest | <$0.10/mo |
| Secret Manager            | 4 secrets, ≤6 free                      | $0          |
| GCS (`sajni-blobs`)       | Standard, single region, <1GB           | <$0.05/mo |
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
```

### Secrets

```sh
echo -n "postgres://USER:PASS@HOST:5432/sajni?sslmode=require" \
  | gcloud secrets create sajni-database-url --data-file=-

openssl rand -hex 32 \
  | gcloud secrets create sajni-jwt-secret --data-file=-

# Optional integrations. Create the secret even if empty — the
# deploy workflow references all four unconditionally.
echo -n "your-tmdb-key"   | gcloud secrets create sajni-tmdb-key   --data-file=-
echo -n "your-gemini-key" | gcloud secrets create sajni-gemini-key --data-file=-
```

Rotate later with `gcloud secrets versions add NAME --data-file=-`.
The deploy uses `:latest` so a fresh revision picks up rotations.

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
```

### GitHub configuration

In **Settings → Secrets and variables → Actions** of `ohmysajni/sajni-api`,
add these as **Variables** (not Secrets — they're identifiers, not credentials):

| Variable              | Value                                                                                            |
| --------------------- | ------------------------------------------------------------------------------------------------ |
| `GCP_PROJECT_ID`      | `ohmysajni`                                                                                      |
| `GCP_REGION`          | `asia-south1`                                                                                    |
| `GCP_SERVICE_ACCOUNT` | `sajni-deployer@ohmysajni.iam.gserviceaccount.com`                                               |
| `GCP_WIF_PROVIDER`    | the line you `echo`d at the end of the IAM block                                                 |
| `GCS_BUCKET`          | `sajni-blobs`                                                                                    |
| `CORS_ORIGIN`         | `https://ohmysajni.com` (the frontend's production origin)                                       |

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
4. Hits `/healthz` on the new revision; fails the workflow if it
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
