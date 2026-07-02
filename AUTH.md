# Sajni authentication — end-to-end guide

This document walks through every piece of Sajni's auth system: the
three sign-in methods, how accounts are linked, every decision branch
along the way, and the concepts behind them. It assumes no prior
background in OAuth, JWTs, or token-based auth — each term is introduced
the first time it appears.

---

## 1. The vocabulary (one-time primer)

Before reading the flow diagrams, it helps to have a shared meaning for
a handful of terms.

- **Identity**: A claim of "this is who I am" issued by some authority.
  In Sajni an identity is one of three things: a Google account, a
  GitHub account, or an email address that proved control of itself by
  reading a one-time code we sent. Each identity is stored as one row
  in the `auth_identities` table.

- **User account**: The actual person inside Sajni — their notes, tasks,
  habits, finance. Stored as one row in the `users` table. A user
  account can have **multiple identities pointing at it**. That is the
  "account linking" feature: when you sign in with Google and GitHub
  using the same email address, both end up attached to the same user
  account.

- **Verified email**: A provider (Google or GitHub) tells us "we
  confirmed this person owns this email address." Without that
  confirmation, an attacker could create a Google account that claims
  any email address they want — so we don't trust it for linking.

- **JWT (JSON Web Token)**: A short string that contains a few claims
  (e.g. "user id = abc, expires at 12:34") signed with a secret only the
  server knows. The frontend sends it on every API call. We verify the
  signature and read the user id without any database lookup. JWTs are
  *stateless* — there's no "is this token revoked?" table to check, so
  they have to be short-lived.

- **Access token**: A JWT that grants API access. In Sajni it expires
  after **30 minutes**. The frontend stores it in memory only — never
  in `localStorage` or a cookie — so XSS can't steal it from disk.

- **Refresh token**: A long-lived random string (32 bytes, base64-url
  encoded) stored as an `httpOnly` cookie. Because the browser sets
  `httpOnly` on the cookie, JavaScript on the page can't read it — even
  if an attacker injected a script. When the access token expires, the
  frontend POSTs to `/api/auth/refresh`; the cookie travels along
  automatically, the server looks it up, issues a fresh access token,
  rotates the refresh token, and the user never notices. In Sajni
  refresh tokens are valid for **7 days**.

- **SHA-256 hash**: A one-way fingerprint of a string. Two inputs that
  are byte-identical produce the same fingerprint; reversing the
  fingerprint to get back the input is computationally infeasible.
  Sajni stores `SHA-256(refresh_token)` instead of the raw token, so a
  database leak can't be used to sign in as anyone. (The token itself
  lives only in the browser cookie.)

- **OAuth 2.0 / OpenID Connect**: An industry protocol where you, the
  user, log into a provider (Google) and the provider hands our app
  (Sajni) a one-shot **authorization code**. Sajni then exchanges that
  code with the provider for the user's identity details (subject id +
  email + verification flag). Sajni never sees the user's Google
  password.

- **TOTP code** (in our usage): A 6-digit one-time number generated
  cryptographically, sent via email through Resend, and valid for 10
  minutes. "TOTP" usually means time-based codes from an authenticator
  app; we use the same shape (6 digits, short TTL, single use) but
  delivery is email, not an app.

- **CITEXT**: A PostgreSQL column type for case-insensitive text. We
  use it for email so `Alice@x.com` and `alice@x.com` are the same row.

- **UUIDv7**: A 128-bit identifier whose first 48 bits encode the
  millisecond timestamp at creation. Sortable by time (insert locality
  helps the B-tree index), opaque to enumeration (no /users/1, /users/2
  scraping), generated in Go so the database doesn't need an extension.

---

## 2. The data model

Four tables form the auth surface (see `internal/db/db.go`):

```
users
  id            uuid       PK  (UUIDv7, minted in Go)
  email         citext     UNIQUE NOT NULL
  name          text       NOT NULL DEFAULT ''
  onboarded_at  timestamptz NULL   — null until the first-run tour ends
  created_at    timestamptz NOT NULL DEFAULT now()
  deleted_at    timestamptz NULL   — set during the 7-day grace before purge

auth_identities                   — one row per (provider, subject) pair
  id               uuid     PK
  user_id          uuid     FK → users(id) ON DELETE CASCADE
  provider         text     'google' | 'github' | 'email'
  provider_subject text     google's `sub`, github's id, or the email
  email            citext   last-seen email from this provider
  created_at       timestamptz
  last_used_at     timestamptz
  UNIQUE (provider, provider_subject)
  INDEX  (user_id)

email_codes                       — single-use 6-digit codes
  id            uuid       PK
  email         citext     NOT NULL
  code_hash     bytea      SHA-256 of the 6-digit code
  purpose       text       'login' | 'link'
  link_user_id  uuid       NULL  — set when purpose='link'
  link_provider text       NULL
  link_subject  text       NULL
  link_name     text       NULL
  attempts      integer    NOT NULL DEFAULT 0
  expires_at    timestamptz NOT NULL
  consumed_at   timestamptz NULL
  created_at    timestamptz NOT NULL DEFAULT now()
  INDEX (email, expires_at) WHERE consumed_at IS NULL

refresh_tokens
  id          uuid       PK
  user_id     uuid       FK → users(id) ON DELETE CASCADE
  token_hash  bytea      SHA-256(raw refresh token), UNIQUE
  expires_at  timestamptz NOT NULL
  created_at  timestamptz NOT NULL DEFAULT now()
  INDEX (user_id)
```

Why these shapes:

- `users.id` is UUIDv7, not `BIGSERIAL`. Time-sortable so B-trees stay
  hot, and not enumerable so `/api/users/1` can't be probed.
- `email` is CITEXT so `Alice@x` and `alice@x` collide on the UNIQUE.
- `auth_identities` is the "linking" table: one user, many identities.
- `email_codes.code_hash` stores the SHA-256 of the code, not the code
  itself, so a database leak doesn't reveal active codes.
- `refresh_tokens.token_hash` is SHA-256 of the raw token, indexed
  UNIQUE. That gives us one indexed equality lookup per refresh — no
  per-row bcrypt loop. Refresh latency stays under 5 ms regardless of
  fleet size.

---

## 3. The three sign-in methods

### 3a. Google (OAuth 2.0 + OpenID Connect)

1. User clicks "Continue with Google" on `/signin`.
2. Frontend `beginOAuth('google')` calls `window.location.href =
   ${API_BASE}/api/auth/google/start`. We use a full page navigation —
   not `fetch` — because the OAuth flow needs to set provider cookies
   on the user's browser (state, anti-CSRF), and a JS fetch can't carry
   the 302 chain through.
3. Server `oauthStart("google")` builds a stateless, signed `state`
   value of the shape `<nonce>.<expUnix>.<hmacSHA256>` (HMAC keyed by
   `JWT_SECRET`, 10-minute expiry) and 302s the browser to Google's
   consent screen with scopes `openid email`.
   - **Why `state`**: prevents CSRF. When Google calls us back, the
     callback URL includes `?state=...`. We re-derive the HMAC with
     `JWT_SECRET` and reject the request if the signature doesn't
     match or the value is expired.
   - **Why HMAC and not a cookie?** A cookie-based scheme breaks the
     moment the API host and the web origin are on different eTLD+1s
     (Cloud Run's auto-assigned `*.run.app` vs the user's
     `ohmysajni.com`). Modern browsers' tracking-protection / CHIPS
     modes drop the `SameSite=None; Secure` cookie on the cross-site
     callback, so every login fails with `state mismatch`. The signed
     state survives any cookie policy because the proof of authenticity
     travels in the URL itself, not in a cookie.
   - **Why no `profile` scope**: that scope is what makes Google's
     consent screen ask for "View your profile picture." We don't want
     that prompt — Sajni doesn't store avatars. The trade-off is we
     also don't get a real display name, so we fall back to the email
     local-part (`alice` from `alice@x.com`).
4. User approves on Google. Google 302s back to
   `https://api.ohmysajni.com/api/auth/google/callback?code=...&state=...`.
5. Server `oauthCallback("google")`:
   - Verifies the signed `state` HMAC + expiry → reject if invalid
     ("state mismatch").
   - Exchanges `code` for an access token at Google's `/token`
     endpoint, then calls Google's userinfo endpoint to read `sub` +
     `email` + `email_verified`.
   - Calls `resolveOrLinkIdentity(...)` — see Section 4.
   - Issues access + refresh tokens, sets the refresh cookie, 302s the
     browser to `${APP_URL}/auth/done?linked=google#access=<jwt>`.
     In production, OAuth should start and callback through the
     frontend's same-origin `/api` rewrite so this cookie is scoped to
     the app host; otherwise a later `/api/auth/refresh` after page
     reload will not receive the cookie.
     - The access JWT goes in the **URL fragment** (`#...`), not the
       query string. Fragments are not sent in the `Referer` header
       and not logged by most server logs.
     - `?linked=google` is only set if the API just attached a new
       identity to an existing user account (so the frontend can fire a
       toast: "Linked google to your account").
6. Frontend `/auth/done` (`pages/Auth/OAuthDone.tsx`) reads the
   fragment, hands the token to `AuthContext.hydrateFromAccessToken`,
   clears the fragment from the URL bar, fires the toast if `?linked`
   is present, and navigates to `/`.

### 3b. GitHub (OAuth 2.0)

Identical to Google, except:
- Scopes are `read:user user:email` (we need `user:email` so we can
  read the user's verified primary email even when it's hidden from the
  public profile).
- Provider implementation lives in
  `internal/auth/providers/github.go`. After exchanging the code, we
  hit two endpoints: `/user` for the numeric id and display name, and
  `/user/emails` for the verified primary email.
- `provider_subject` is the GitHub user id as a string (e.g. `"42"`),
  because GitHub guarantees that's stable across renames.

### 3c. Email + Resend-delivered TOTP code

This is the "no provider" path. It exists so people who don't have or
don't want to use Google/GitHub can still sign in.

1. User types their email on `/signin` and clicks "Email me a code".
2. Frontend POSTs `{email}` to `/api/auth/email/start`.
3. Server `handleEmailStart`:
   - Rate-limits to **3 sends per email per hour** (counts rows in
     `email_codes`).
   - Generates a 6-digit code from `crypto/rand`.
   - Stores `SHA-256(code)` in `email_codes` with
     `purpose='login'`, `expires_at = now() + 10 minutes`.
   - Renders the M3-themed HTML template (`email_templates/totp.html`)
     and ships it via Resend's REST API. If `RESEND_API_KEY` is unset
     (local dev), we print the code to stdout instead.
4. User opens the email, sees the code, types it (or pastes it) into
   the 6-digit OTP boxes on `/signin`.
5. Frontend POSTs `{email, code}` to `/api/auth/email/verify`.
6. Server `handleEmailVerify`:
   - Looks up the most recent un-consumed row for that email.
   - Checks the row hasn't expired and hasn't hit 5 wrong attempts.
   - Constant-time compares `SHA-256(typed_code) == stored_hash`. On
     mismatch, increments `attempts`. After 5 mismatches the row is
     locked.
   - Marks the row `consumed_at = now()` (single-use).
   - Calls `findOrCreateByEmail(...)` — creates a fresh user row if the
     email is new, returns the existing one otherwise.
   - Inserts (or no-ops via ON CONFLICT) an `auth_identities` row with
     `provider='email'`, `provider_subject=email`.
   - Issues access + refresh tokens, returns them in a JSON body.

---

## 4. Account linking — the decision tree

The load-bearing function is `resolveOrLinkIdentity` in
`internal/auth/linking.go`. It runs inside a `SERIALIZABLE` transaction
so two concurrent OAuth callbacks for the same email can't race into
creating duplicate users. The inputs are the normalized identity:

```
provider       — "google" | "github" | "email"
subject        — provider's stable id
email          — lowercased
emailVerified  — bool (from the provider)
name           — string (may be empty)
```

The output is `(userID, needsLink, linkedNew, err)`.

```
┌─────────────────────────────────────────────────────────────────────┐
│ Step 1: SELECT user_id FROM auth_identities                          │
│         WHERE provider=$1 AND provider_subject=$2                    │
│                                                                       │
│   HIT  → known identity. UPDATE last_used_at + email.                 │
│          Clear soft-delete grace if any. Return (user_id, false,      │
│          false). This is the steady-state "I've signed in before"     │
│          path — no linking, no merging.                               │
│                                                                       │
│   MISS → continue to Step 2.                                          │
└─────────────────────────────────────────────────────────────────────┘
                                  │
                                  ▼
┌─────────────────────────────────────────────────────────────────────┐
│ Step 2: SELECT id FROM users WHERE email=$1                          │
│                                                                       │
│   MISS → this is a brand-new account.                                 │
│          INSERT INTO users (...). INSERT INTO auth_identities (...). │
│          Return (newID, false, false).                                │
│                                                                       │
│   HIT && emailVerified=true → the provider confirmed the email and   │
│          we already have a user with it. Attach the new identity:    │
│          INSERT INTO auth_identities (...). If users.name was blank, │
│          fill it from the provider. Return (userID, false, true).    │
│          `linkedNew=true` is the signal that the frontend should     │
│          show a toast "linked X to your account".                    │
│                                                                       │
│   HIT && emailVerified=false → DO NOT silently link. An attacker     │
│          could spin up a provider account that claims any unverified │
│          email. Instead, we return (userID, true, false). The HTTP   │
│          handler reacts to `needsLink=true` by sending a TOTP code   │
│          to the email and redirecting to /auth/link, where the real  │
│          owner enters the code. Once verified, the link finalizes    │
│          (see Section 5 below).                                      │
└─────────────────────────────────────────────────────────────────────┘
```

### The link-via-TOTP path in detail

This is the rare-but-critical case. Concrete scenario:

1. Alice signed up with email-TOTP using `alice@x.com`.
2. Months later she tries "Continue with GitHub". Her GitHub account
   has `alice@x.com` listed but unverified.
3. If we linked silently, anybody who later added `alice@x.com` to
   their own GitHub account could take over Alice's Sajni account. We
   must reject that.
4. So the API: stashes the inbound identity (provider, subject, name)
   in the `email_codes` row with `purpose='link'` and
   `link_user_id=alice.id`, ships a TOTP to `alice@x.com`, and 302s
   the browser to `/auth/link`.
5. Alice opens the email, enters the code. The verify handler sees
   `purpose='link'`, pulls the stashed identity, and INSERTs the
   `auth_identities` row scoped to her existing `user_id`.
6. Issues a fresh session. Done — Alice can now sign in with either
   email or GitHub.

### Every scenario, mapped

| Scenario | Provider says | DB before | DB after | Outcome |
|---|---|---|---|---|
| First-ever sign-in via Google | `verified=true` | nothing | new `users` row + 1 `auth_identities` (google) | new account, returns access token |
| First-ever sign-in via GitHub | `verified=true` | nothing | new `users` row + 1 `auth_identities` (github) | new account |
| First-ever sign-in via email-TOTP | n/a | nothing | new `users` + 1 `auth_identities` (email) after verify | new account |
| Sign in again via Google (same Google) | `verified=true` | `(google, sub)` row | `last_used_at` bumped | same account, no toast |
| Sign in again via GitHub (same GitHub) | `verified=true` | `(github, id)` row | `last_used_at` bumped | same account, no toast |
| Sign in via Google when only GitHub existed (same email, both verified) | `verified=true` | 1 identity (github) | + 1 identity (google) | **same account**, toast "linked google" |
| Sign in via GitHub when only email existed (same email, GitHub verified) | `verified=true` | 1 identity (email) | + 1 identity (github) | same account, toast "linked github" |
| Sign in via GitHub but `verified=false`, email matches existing user | `verified=false` | 1 identity (anything) | unchanged until TOTP | TOTP sent → /auth/link → after verify, identity row added |
| Sign in via email-TOTP when only Google existed, same email | n/a | 1 identity (google) | + 1 identity (email) | same account |
| Two different people, same email | n/a — can't happen | — | — | Providers won't issue a verified email they don't own. Email-TOTP needs control of the inbox. So two strangers can't collide. |
| User changes their email at Google | `verified=true` for new email | identity row stores old email | row's `email` column updated, `user_id` unchanged | same Sajni account |
| User in 7-day soft-delete grace signs in again | `verified=true` | row has `deleted_at` set | `deleted_at = NULL` | account un-deleted |

---

## 5. The session lifecycle

After a successful sign-in (any of the three paths), `issueSession`:

1. Mints a 30-minute access JWT signed with `JWT_SECRET`.
2. Mints a random 32-byte refresh token, stores `SHA-256(it)` in
   `refresh_tokens` with a 7-day TTL, and sets it as `httpOnly` cookie
   `sajni_refresh` scoped to `/api/auth`.
3. Returns `{ access_token, user }` JSON (or, for OAuth, redirects with
   the access token in the URL fragment).

When the access JWT expires the frontend's `authFetch` sees a 401 and
calls `/api/auth/refresh` automatically. The handler:

1. Reads the cookie, hashes the raw token, runs
   `DELETE … WHERE token_hash=$1 AND expires_at > now() RETURNING
   user_id`. One indexed lookup, one row mutation. No bcrypt sweep.
2. If found, mints a fresh access token AND a fresh refresh token,
   rotates the cookie, and returns the new pair.
3. If not found (token reused, expired, or unknown) → 401 + clears the
   cookie. The frontend bounces to `/signin`.

**Rotation** is important: every refresh invalidates the previous
refresh token. If an attacker steals the cookie and uses it, the next
legitimate refresh from the real user fails, which is an observable
sign of compromise. (Sajni doesn't currently alert on this, but the
data is there to add it later.)

Logout (`POST /api/auth/logout`):

1. Best-effort `DELETE FROM refresh_tokens WHERE token_hash=$1`.
2. Clears the cookie.
3. Returns 200.

---

## 6. Frontend wiring (quick map)

- `src/auth/client.ts` — `authFetch` (auto-401-retry-with-refresh),
  `requestJSON`, in-memory `accessToken`.
- `src/auth/AuthContext.tsx` — React context exposing `user`,
  `startEmail`, `verifyEmailCode`, `beginOAuth`, `hydrateFromAccessToken`,
  `markOnboarded`, `updateName`, `logout`. On mount it does a one-shot
  `/auth/refresh` so a returning user with a live cookie skips the
  sign-in page.
- `src/pages/Auth/SignIn.tsx` — the single sign-in page. OAuth
  buttons + email-TOTP form.
- `src/pages/Auth/OAuthDone.tsx` — the OAuth callback landing page.
  Reads the URL fragment, hands the token to the auth context.
- `src/pages/Auth/LinkChallenge.tsx` — the TOTP-link page for the
  "provider returned an unverified email collision" path.
- `src/auth/RequireAuth.tsx` — wraps protected routes; bounces to
  `/signin` when there's no user.
- `src/components/Onboarding.tsx` — the first-run tour with sidebar
  popovers, gated on `user.onboarded_at === null`.

---

## 7. Threat model — what each control defends against

| Threat | Defense |
|---|---|
| Stolen DB dump | Refresh tokens stored as SHA-256 → can't replay. Codes stored as SHA-256 → can't be used. Passwords aren't stored at all (we removed bcrypt). |
| XSS reading the access token | Token lives in JS memory, but never in `localStorage`. Worst case: a session that survives only until the next refresh. Refresh cookie is `httpOnly` so XSS can't extend the session. |
| CSRF on the OAuth flow | Stateless HMAC-signed `state` with short expiry in `oauthCallback`. |
| CSRF on the refresh endpoint | Refresh cookie is `httpOnly`, `Secure` in prod, and scoped to `/api/auth`. Production uses the frontend same-origin `/api` rewrite so the browser sends it on refresh without third-party-cookie dependence. |
| Replayed refresh token | Refresh-token rotation deletes the row on every successful refresh; a second use 401s. |
| Email enumeration via "is this email registered?" timing | `/auth/email/start` returns 200 for any well-formed email so an attacker can't differentiate. Existence is only revealed implicitly by whether the user can complete the flow (which they can — by registering). |
| Brute-forcing the 6-digit code | 5 attempt cap per code, 10-minute TTL, single use. 1 in 1,000,000 odds and only 5 shots = vanishingly small per-code. Rate limit of 3 codes per email per hour caps the rate of attempts. |
| Account takeover via spoofed unverified email | TOTP-link challenge — Section 4. |
| Enumeration via sequential ids | UUIDv7. Time-sortable, not guessable. |
| Long-running sessions on a forgotten device | Refresh expires after 7 days; the device is signed out automatically. Logout removes the refresh row immediately. |

---

## 8. Common bugs we have hit (and the SQL story behind them)

These are worth knowing because they show up as opaque
`(SQLSTATE 4XXXX)` errors in the API response.

- `SQLSTATE 42P18 — could not determine data type of parameter $N`.
  pgx tried to infer the type of placeholder `$N` and Postgres
  couldn't. Two causes we've hit:
  1. The Go call passed an argument but the SQL never references that
     `$N`. The query "needs" the parameter (count must match) but no
     column anchors its type. Fix: drop the extra arg or reference the
     placeholder.
  2. A nullable expression like `NULLIF($5,'')` feeds a column whose
     type the planner can't see through the function call. Fix: cast
     explicitly, e.g. `NULLIF($5,'')::uuid`.

- `SQLSTATE 42P08 — inconsistent types deduced for parameter $N`.
  The same `$N` appears in two positions whose columns have different
  types (e.g. `TEXT` and `CITEXT`). Postgres can't pick one type for
  the parameter. Fix: use two separate parameters, even if you pass
  the same Go value to both.

- `SQLSTATE 42804 — column "X" is of type uuid but expression is of
  type text`. The Go value is a `string` and the column expects
  `uuid`. The CITEXT and TEXT cases auto-coerce; UUID does not. Fix:
  cast at the parameter site or at the column.

---

## 9. Operational notes

- **Env vars on the API**: `JWT_SECRET`, `DATABASE_URL`, `APP_URL`,
  `API_BASE_URL`, `CORS_ORIGIN`, `COOKIE_INSECURE` (set to `1` in
  local dev only), `GOOGLE_OAUTH_CLIENT_ID`,
  `GOOGLE_OAUTH_CLIENT_SECRET`, `GITHUB_OAUTH_CLIENT_ID`,
  `GITHUB_OAUTH_CLIENT_SECRET`, `RESEND_API_KEY`, `EMAIL_FROM`.
  With the Vercel `/api` rewrite, register OAuth callbacks on
  `${APP_URL}/api/auth/{provider}/callback`; `API_BASE_URL` is only the
  direct Cloud Run fallback.
  Current DNS redirects `ohmysajni.com` to `www.ohmysajni.com`, so prod
  `APP_URL` should be `https://www.ohmysajni.com`.
- **Local auth bypass**: `DEV_AUTH_BYPASS=1` makes
  `/api/auth/refresh` mint a normal session for
  `DEV_AUTH_BYPASS_EMAIL` (default `dev@sajni.local`) when the request
  comes from `localhost`, `.local`, or a private-network origin such as
  `http://192.168.x.x:5173`. This is for local demos, AI agents, and
  phone testing through `vite --host`; never set it in production. In
  this mode CORS also reflects localhost/private-network origins so
  direct LAN API calls work when `CORS_ORIGIN` is pinned.
- **Env vars on the web**: `VITE_API_URL` only. The frontend never
  needs the OAuth client ids.
- **Schema reset**: `DROP_AND_RESEED=1 go run ./cmd/...` wipes the
  public schema and re-runs the migrate. Use sparingly; flip back off
  after the next boot.
- **Resend domain**: until a verified domain is configured, use
  `onboarding@resend.dev` as `EMAIL_FROM`. The user will see "via
  resend.dev" — fine for staging.
- **Cleanup jobs not yet implemented**: expired `email_codes`,
  expired `refresh_tokens`, and `users` past their soft-delete grace
  are not garbage collected by a background job. They accumulate.
  Adding a daily prune is a small follow-up.
