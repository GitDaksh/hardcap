# Hardcap

An OpenAI-compatible gateway that makes surprise AI bills impossible.

Your apps send their AI requests to Hardcap instead of straight to the provider. Every project gets a spending cap, and Hardcap guarantees the project can never spend past it: not when hundreds of requests arrive in the same millisecond, not when a provider times out mid-reply, not when the gateway itself is killed with `kill -9`.

![Two projects with a $1.00 cap after 200 clients hit both at once: the naive one ends at $1.1568, Hardcap's at $0.9832](docs/dashboard.png)

*The built-in demo: two projects with a $1.00 cap, 200 concurrent clients each. The one that checks its balance and charges afterwards (how most apps do it) ends 15.7% over. Hardcap's never crosses the line.*

## The problem

AI providers bill per token, after the fact. The obvious budget check is "is there money left? then send the request, and charge it when it finishes". Under load that check is a race: 200 requests read the same balance at the same moment, all see "plenty left", and all go through. The cap is a suggestion.

## How Hardcap works

Before a request is sent, Hardcap **holds** the most it could possibly cost. When the reply finishes, it **settles**: releases the hold and charges what the request really cost.

```
app ──► Hardcap ─────────────────────────────────────────────► provider
          1. check the API key
          2. rate limit (token buckets in Redis)
          3. HOLD the worst-case cost (Postgres) ── doesn't fit? 402, nothing sent
          4. send to the first healthy provider, stream the reply back,
             read the real token usage on the way
          5. SETTLE: release the hold, charge the real cost
```

The worst case is knowable before anything is sent, because Hardcap bounds both sides of the bill:

- **Output:** every request gets a `max_tokens` (the model's default if the client didn't set one), so a reply can't run forever.
- **Input:** a prompt can't be more tokens than the request has bytes, plus a fixed allowance for the provider's chat template (`prompt_overhead_tokens`).
- **Price:** the most expensive of the model's routes, in case a request falls back to a pricier provider.

The invariant, checked under row locks in a single Postgres function:

```
spent + held + this request's worst case ≤ cap        (for the key, and for its project)
```

Money is stored as integer micro-dollars ($1 = 1,000,000), and costs round up.

## When things go wrong

Most of the work is in the failure cases. Each row is covered by a test.

| What happens | What Hardcap does | Why |
|---|---|---|
| 1,000 requests arrive at once | Exactly as many as fit get through | Hold and check are one atomic step. 1,000 concurrent $0.003 holds on a $1 cap: exactly 333 succeed. |
| Provider fails before answering (DNS, connection refused, 429, 5xx) | Retries with jittered backoff, then falls back to the next provider | Nothing was generated, so nothing is owed |
| Provider times out, or the connection drops mid-reply | No retry. Charges the full hold. | Tokens may have been generated and billed. A retry could pay twice. |
| Reply finishes without a usage report | Charges the full hold | Unknown cost is charged at the worst case, never at zero |
| Client hangs up mid-stream | Cancels the provider call, charges the full hold | The provider may already have billed for what it generated |
| Gateway is killed (`kill -9`) mid-request | Holds stay held. When they expire, any gateway's sweeper charges them in full. | The test kills a real gateway process mid-stream, then audits the books |
| Gateway is stopped (`SIGTERM`) | Stops taking requests, lets the ones in flight finish and settle | Tested with a real process too |
| Settle runs twice (retry after a network blip) | The second one changes nothing | Receipts are unique per request |
| Postgres is down | Refuses every request with 503: **fails closed** | Budgets protect money |
| Redis is down | Skips rate limits: **fails open** | Rate limits protect availability |
| A provider keeps failing | Its circuit breaker opens: requests skip it for 30s, then one trial request | Don't wait on a dead provider |

## Quickstart (macOS)

You need [Homebrew](https://brew.sh). The fake AI provider is built in, so none of this costs anything.

```bash
# 1. Install Go, Postgres and Redis, and start them (once)
brew install go postgresql@18 redis
brew services start postgresql@18
brew services start redis
echo "export PATH=\"$(brew --prefix postgresql@18)/bin:\$PATH\"" >> ~/.zshrc
source ~/.zshrc

# 2. Get the code and create the databases
git clone https://github.com/GitDaksh/hardcap.git
cd hardcap
make setup

# 3. Start the fake provider and Hardcap
make run
```

Open <http://localhost:8080/dashboard/>, paste the admin token `dev-admin-token`, and click **Run the demo**.

In a second terminal, `make stampede` runs the same race from the command line and prints a report. `make test` runs every test (it needs Postgres and Redis running). `make` on its own lists every command.

On Linux, install Go 1.26 or newer, Postgres and Redis your usual way, then pass your database URL: `make run DATABASE_URL=postgres://user:password@localhost:5432/hardcap`.

## Use it from your app

Create a project and an API key, in the dashboard (New project, then Create key) or with the admin API:

```bash
curl localhost:8080/admin/api/projects -H "Authorization: Bearer dev-admin-token" \
  -d '{"name": "my app", "cap_usd": 5}'
# {"id":1,"name":"my app","mode":"hardcap","cap_micros":5000000,...}

curl localhost:8080/admin/api/projects/1/keys -H "Authorization: Bearer dev-admin-token" \
  -d '{"name": "laptop"}'
# {"key":{...},"secret":"hc_9TcLsa1wM5aSKVhjo3tWA-5pNd-Kk1UD"}
```

The secret is shown once. Hardcap stores only its SHA-256 hash.

Then point any OpenAI client at Hardcap:

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="hc_...")
reply = client.chat.completions.create(
    model="mock-1",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(reply.choices[0].message.content)
```

Every reply says what was held and what was charged:

```
HTTP/1.1 200 OK
Server-Timing: auth;dur=0.587, ratelimit;dur=1.293, reserve;dur=1.008, admission;dur=2.932
X-Hardcap-Held-Usd: 0.002580
X-Hardcap-Cost-Usd: 0.000770
X-Ratelimit-Limit-Requests: 6000
X-Ratelimit-Remaining-Requests: 5999
X-Request-Id: req_cb6e76f4f282bb17
```

(`X-Hardcap-Cost-Usd` is on whole replies only. A stream's headers go out before its cost is known.)

When a request doesn't fit, nothing is sent to the provider, and the client gets an error in OpenAI's format, so existing error handling works:

```
HTTP/1.1 402 Payment Required

{"error":{"code":"budget_exceeded","type":"insufficient_quota","message":"project budget
exhausted: this request could cost up to $0.002580, but only $0.002490 of the $0.005000
cap is free"}}
```

### Real models, still free

`hardcap.yaml` includes two real models on [Groq's free plan](https://console.groq.com/docs/rate-limits) (about 30 requests a minute and 1,000 a day each): `gpt-oss-20b` and `gpt-oss-120b`. Create a free API key at <https://console.groq.com/keys>, then:

```bash
GROQ_API_KEY=gsk_... make run
```

To run a model on your own computer instead, see the `local` model at the end of `hardcap.yaml` (it uses [Ollama](https://ollama.com)).

## Configuration

`hardcap.yaml` lists the providers, the models clients can ask for, each model's routes (tried in order), and what each route costs per million tokens. A provider whose API key isn't set is switched off with a warning, and so is any model left with no providers.

The gateway reads its settings from environment variables (`make run` sets the first ones for you):

| Variable | Default | What it does |
|---|---|---|
| `HARDCAP_DATABASE_URL` | `postgres://localhost:5432/hardcap?sslmode=disable` | Where budgets live. Tables are created on startup. |
| `HARDCAP_REDIS_URL` | *(empty: no rate limits)* | Redis for rate limits |
| `HARDCAP_ADMIN_TOKEN` | *(empty: no admin API or dashboard)* | Token for the admin API and dashboard, 8+ characters |
| `HARDCAP_DEMO` | `false` | Turns on the dashboard's Stampede button |
| `HARDCAP_ADDR` | `localhost:8080` | Where Hardcap listens (`:8080` for every interface) |
| `HARDCAP_CONFIG` | `hardcap.yaml` | The config file |
| `HARDCAP_DB_MAX_CONNS` | `20` | Postgres connection pool size |
| `HARDCAP_REQUEST_TIMEOUT` | `5m` | The longest one request may run |
| `HARDCAP_HOLD_GRACE` | `1m` | How long after the timeout a hold may be charged by the sweeper |
| `HARDCAP_SWEEP_INTERVAL` | `15s` | How often each gateway looks for expired holds |
| `HARDCAP_SHUTDOWN_GRACE` | `25s` | How long requests in flight get to finish on shutdown |
| `GROQ_API_KEY` | *(empty: Groq models off)* | Your Groq key |

The fake provider has its own: `MOCK_ADDR` (`localhost:8090`), `MOCK_TOKEN_DELAY` (`30ms` per token), `MOCK_REPLY_TOKENS` (`24`) and `MOCK_FAIL_RATE` (`0`, from 0 to 1).

### Admin API

Every call needs `Authorization: Bearer <HARDCAP_ADMIN_TOKEN>`.

| Call | What it does |
|---|---|
| `GET /admin/api/projects` | List projects with spent, held and cap |
| `POST /admin/api/projects` | Create one: `{"name", "cap_usd", "mode": "hardcap" or "naive"}` |
| `PATCH /admin/api/projects/{id}` | Change `cap_usd` or `mode` |
| `POST /admin/api/projects/{id}/reset` | Set spending back to $0 and delete the receipts |
| `GET`, `POST /admin/api/projects/{id}/keys` | List keys, or create one: `{"name", "cap_usd", "rpm_limit", "tpm_limit"}` (all but the name optional) |
| `POST /admin/api/keys/{id}/revoke` | Revoke a key. Takes effect immediately. |
| `GET /admin/api/projects/{id}/usage?limit=50` | Recent receipts: tokens, held, charged, outcome |
| `GET /admin/api/projects/{id}/audit` | Recount the books from receipts and open holds |
| `POST /admin/api/projects/{id}/stampede` | Start a stampede: `{"workers", "seconds", "model"}` (needs `HARDCAP_DEMO=1`) |

Naive mode exists only to show what goes wrong without Hardcap. It checks the balance and charges afterwards, like typical budget code.

### Metrics

`GET /metrics` serves Prometheus metrics, including `hardcap_requests_total{model,outcome}`, `hardcap_admission_duration_seconds` (Hardcap's own overhead), `hardcap_spend_micros_total`, `hardcap_budget_rejections_total{scope}`, `hardcap_upstream_attempts_total{provider,result}`, `hardcap_breaker_state{provider}`, `hardcap_holds_charged_on_expiry_total` and `hardcap_hold_exceeded_total`. `GET /healthz` says the process is up. `GET /readyz` says it can reach Postgres.

## Benchmarks

Measured on a 2-vCPU Linux VM with Postgres 16 (fsync on) and Redis 7. Your machine will give different numbers: run `make stampede` and `make bench` to get your own.

**The stampede** (`make stampede`): 200 streaming clients against one project with a $1.00 cap, until it's spent. Each reply takes about 0.75 seconds.

| | Naive | Hardcap |
|---|---|---|
| Requests served | 1,446 | 1,247 |
| Refused with 402 | 2,000 | 2,700 |
| Spent | $1.1568 | $0.9976 |
| Over the cap | +$0.1568 (15.7%) | never |
| Books balance | yes | yes |
| Gateway overhead per request, p50 / p99 | 0.18 / 10 ms | 3.8 / 89 ms |

Hardcap serves fewer requests on purpose. Each one holds its worst case ($0.00286 here) while it runs, though it really costs about $0.0008. With 200 in flight, over half the budget is held at any moment, so near the end Hardcap refuses requests that would probably have fit. That's the price of a guarantee.

The overhead is higher because Hardcap writes the hold to disk before the request goes out. The p99 comes from all 200 clients firing in the same millisecond at one project, so their holds take turns. On a quiet project the whole admission (key check, rate limit, hold) takes about 0.7 ms.

**The ledger alone** (`make bench`): one project, N clients, each doing a hold and a settle as fast as it can.

| Clients | Before per-project queueing | After |
|---|---|---|
| 1 | 1,825 requests/s | 1,846 requests/s |
| 8 | 907 requests/s | 1,876 requests/s |
| 32 | 225 requests/s | 1,950 requests/s |

## Design notes

**Postgres holds the money, Redis only the rate limits.** A budget has to survive crashes and stay exact, so it lives in Postgres, in tables you can audit (`GET .../audit` recounts every project from its receipts). Rate limit buckets can be lost without harm, so they live in Redis, as one atomic Lua script per request.

**One function, one round trip.** `hardcap_reserve` checks and holds in a single PL/pgSQL call while holding row locks on the key and the project, so no other request can slip in between "check" and "spend". Every function locks the key first and the project second, so they can't deadlock on each other.

**A deadlock the stress tests found.** The first version locked rows `FOR UPDATE`. Inserting a hold makes Postgres check its foreign keys, which takes a `FOR KEY SHARE` lock on the same rows, and `FOR UPDATE` conflicts with that. Under sustained load, two requests could wait on each other. The fix was `FOR NO KEY UPDATE`, the lock a normal `UPDATE` takes.

**A slowdown the benchmark found.** Every request on a project locks that project's row twice. With 32 clients on one project, throughput fell to an eighth of a single client's: a long queue of transactions on one row lock is slow to hand the lock along. Each gateway now queues its own writes to a project in memory, so Postgres sees one transaction per project per gateway (see the table above). Separately, a request that can't fit is refused after a look that takes no locks and writes nothing, so a crowd of refusals stays out of the way of the requests that fit.

**Unknown cost means full cost.** A timeout, a dropped stream, a missing usage report or a crashed gateway all leave the real cost unknown, and the provider may have billed for every token. Hardcap charges the full hold in each case. Overcharging the budget is safe. Undercharging it is how caps break.

**Retry only when nothing was generated.** A refused connection or a 503 proves the provider didn't run the request, so retrying or falling back is free. A timeout proves nothing, so Hardcap doesn't retry it.

**Fail closed for money, open for availability.** Without Postgres, Hardcap can't check budgets, so it refuses everything. Without Redis, it skips rate limits and keeps serving.

## Tests

The tests run against real Postgres and Redis, each test in its own schema. GitHub Actions runs them on every push with the race detector on, then runs a real stampede that fails the build if a Hardcap project ever ends above its cap.

Some of what they prove:

- 1,000 concurrent holds of $0.003 on a $1 cap: exactly 333 succeed
- 200 workers holding and settling for seconds never cross the cap, and the books balance
- the same load through a naive project overspends, and through Hardcap doesn't
- `kill -9` the real gateway process during 10 streams: a fresh gateway's sweeper charges every orphaned hold in full, and the books balance
- `SIGTERM` during 5 streams: all 5 finish and settle at their real cost before the process exits
- a timeout is never retried, a refused connection is, and a breaker opens after failures in a row
- Redis down: requests still flow. Postgres down: every request is refused.
- a refusal doesn't wait for row locks held by others

## Limitations

- **One project's throughput has a ceiling.** Writes to one project's budget take turns, at about 1,900 requests a second on the benchmark VM. Different projects don't wait on each other.
- **Holds are pessimistic.** Near the cap, Hardcap refuses requests that would probably have fit, because it only admits what fits at its worst case.
- **Caps are lifetime totals.** There are no monthly periods yet. Reset a project to start over.
- **Text only, one reply per request.** Images and audio are refused because their cost can't be bounded up front, and `n` must be 1.
- **Prices are set by hand** in `hardcap.yaml`.
- **The dashboard is a local tool.** It keeps the admin token in the browser. Don't expose the admin API to the internet without TLS in front of it.

## Roadmap

- [x] Phase 1: streaming proxy and fake provider
- [x] Phase 2: API keys and hard budget caps (hold, settle, capture)
- [x] Phase 3: rate limits, real providers, retries, fallback and circuit breakers
- [x] Phase 4: load tests, crash tests, metrics
- [x] Phase 5: dashboard, stampede demo, benchmarks, CI
- [ ] Group commit: turn a queue of holds on one project into one round trip
- [ ] Monthly budget periods
- [ ] A Docker Compose file for a one-command start
- [ ] OpenTelemetry traces

## Project layout

```
cmd/hardcap         the gateway: config, Postgres, Redis, sweeper, HTTP server
cmd/mockllm         a fake OpenAI-compatible provider, free to run
cmd/stampede        the load test that proves the cap holds
internal/gateway    the request path: parse, rate limit, hold, route, relay, settle
internal/ledger     budgets: projects, keys, holds, receipts, audits
internal/db         the connection pool, migrations and the SQL functions
internal/ratelimit  token buckets in Redis
internal/upstream   circuit breaker, retry rules, backoff
internal/admin      the admin API and the dashboard (plain JavaScript, no build step)
internal/loadgen    concurrent request generator with latency percentiles
internal/metrics    Prometheus metrics, no dependencies
internal/pricing    money as integer micro-dollars
internal/config     loading and checking hardcap.yaml
internal/mock       the fake provider
internal/testenv    real Postgres and Redis for tests
```

## License

MIT
