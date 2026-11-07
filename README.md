# Hardcap

An OpenAI-compatible gateway that makes surprise AI bills impossible.

Apps send their AI requests to Hardcap instead of straight to the provider. Hardcap forwards each request, measures exactly how many tokens it used, and (from Phase 2) refuses any request that would push a key past its spending cap, even when hundreds of requests arrive at the same moment.

> Work in progress: Phase 1 of 5 is done.

## How it works

```
your app  ──►  Hardcap (:8080)  ──►  AI provider
                                     (the free mock on :8090, for now)
```

- **Streaming passthrough.** Replies stream back word by word, flushed the moment they arrive; nothing is buffered.
- **Usage on every request.** For streams, Hardcap asks the provider for a usage report and hides that extra chunk from clients that didn't ask for it.
- **Hang-ups cancel the provider call.** If the client disconnects, the upstream request is canceled too, so nobody pays for tokens no one will read.
- **Provider keys stay private.** The gateway holds the provider key; clients never see it.

## Run it locally

You need Go 1.24 or newer.

```bash
# Terminal 1: the fake AI provider
go run ./cmd/mockllm

# Terminal 2: the gateway
go run ./cmd/hardcap

# Terminal 3: a streamed request through the gateway
curl -N localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"mock-1","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

The gateway prints one line per request, including the tokens it used.

## Run the tests

```bash
go test -race ./...
```

## Configuration

| Variable | Default | What it does |
|---|---|---|
| `HARDCAP_ADDR` | `localhost:8080` | Where the gateway listens (`:8080` for every interface) |
| `HARDCAP_UPSTREAM_URL` | `http://localhost:8090/v1` | The provider's OpenAI-compatible base URL |
| `HARDCAP_UPSTREAM_KEY` | *(empty)* | The provider's API key (never sent to clients) |
| `MOCK_ADDR` | `localhost:8090` | Where the mock provider listens |
| `MOCK_TOKEN_DELAY` | `30ms` | Pause before each fake token |
| `MOCK_FAIL_RATE` | `0` | Chance (0 to 1) that a request fails with a 500 |
| `MOCK_REPLY_TOKENS` | `24` | Length of a full fake reply, in tokens |

## Project layout

```
cmd/hardcap        the gateway binary
cmd/mockllm        the fake provider binary
internal/gateway   the proxy: forwarding, streaming, usage capture
internal/mock      the fake OpenAI-compatible provider
internal/env       settings from environment variables
internal/serve     HTTP serving with graceful shutdown
```

## Roadmap

- [x] Phase 1: streaming proxy and mock provider
- [ ] Phase 2: API keys and hard budget caps (hold, settle, release)
- [ ] Phase 3: rate limits, real providers, retries and fallback
- [ ] Phase 4: load tests, crash tests, metrics
- [ ] Phase 5: dashboard, benchmarks, demo