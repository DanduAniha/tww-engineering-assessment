# Campaign Events service

A small Go HTTP service that receives delivery-provider webhook events
(`sent`, `delivered`, `opened`, `clicked`) and serves per-campaign statistics for a dashboard.

- Idempotent: providers can retry; the same `event_id` never counts twice.
- Tolerant: one malformed event does not discard the other valid events in a batch.
- Safe under concurrency (tested with the race detector).
- Standard library only. In-memory storage (data is lost on restart).

## Repository layout

```
.
├── README.md  NOTES.md  BUGS.md  AI_USAGE.md  ASSIGNMENT.md
├── service/                 ← the HTTP service (module "campaign-events")
│   ├── main.go              start-up, flags
│   ├── handlers.go          HTTP handlers, status codes, cursor encoding
│   ├── validate.go          per-event decoding + validation + normalisation
│   ├── store.go             in-memory store, dedup, counters (the core logic)
│   ├── types.go             request/response/stored types
│   ├── handlers_test.go     API-level tests (incl. seed file, concurrency)
│   ├── store_test.go        store-level tests (concurrent writers + readers)
│   └── seed/events.json     a day of provider traffic (provided)
└── debugging/               ← the 4-bug exercise, fixes applied (see BUGS.md)
```

## Technology stack
Go 1.22+ (needs the `"POST /events"` method-aware routing of `net/http`), standard library only.
Check with `go version`.

## Architecture in one minute
```
provider ──POST /events──▶ handlers.go ──▶ validate.go (each element on its own)
                                   │
                                   ▼
dashboard ──GET stats────▶ store.go  (one RWMutex)
                              byID:      event_id → event          (duplicate detection)
                              campaigns: campaign → counters + distinct contacts + sorted event list
```
1. The body is first split into raw JSON elements. Each element is then decoded and validated **on its own**,
   so a bad element produces an error entry for that element only.
2. Valid events are applied to the store in **one locked batch**: for each event the store checks `event_id`;
   new → bump the counter; same id and same content → duplicate (ignored); same id, different content → conflict (ignored).
   Checking the id and bumping the counter happen under the same lock, so two simultaneous retries cannot both count.
3. Stats read counters under a read lock. Counters depend only on *which events exist*, never on arrival order, so late and
   out-of-order events give the same numbers.

## Run

```bash
cd service
go run .                      # listens on :8080
go run . -addr :9090          # another port
go run . -max-future-skew 0   # disable the "timestamp in the future" check
```

## API

### `POST /events`
Body: JSON **array** of events. Fixed contract:
```json
[{
  "event_id": "evt_00042",
  "campaign_id": "cmp_summer_sale",
  "contact_id": "ct_017",
  "type": "opened",
  "timestamp": "2026-08-10T09:15:04Z",
  "metadata": {"url": "https://example.com/offer"}
}]
```
Validation: `event_id`, `campaign_id`, `contact_id`, `type`, `timestamp` required (non-empty, ≤256 chars);
`type` ∈ sent|delivered|opened|clicked (case-insensitive); `timestamp` RFC 3339, not more than 24h in the future.

| Status | Meaning |
|---|---|
| `200` | every event was new or a harmless duplicate |
| `207` | some events applied/duplicates, some rejected or conflicting (valid ones **are** stored) |
| `422` | array was readable but nothing in it was usable |
| `400` | body is not a JSON array / not valid JSON |
| `413` | body > 5 MiB or > 5000 events |

Example: one good event, one bad type:
```bash
curl -s -X POST localhost:8080/events -H 'Content-Type: application/json' -d '[
 {"event_id":"evt_1","campaign_id":"cmp_summer_sale","contact_id":"ct_001","type":"delivered","timestamp":"2026-08-10T06:15:00Z"},
 {"event_id":"evt_2","campaign_id":"cmp_summer_sale","contact_id":"ct_002","type":"bounced","timestamp":"2026-08-10T06:16:00Z"}
]'
```
Response `207`:
```json
{"received":2,"accepted":1,"duplicates":0,"conflicts":0,"rejected":1,
 "issues":[{"index":1,"event_id":"evt_2","status":"rejected",
            "errors":["unsupported type \"bounced\" (want sent, delivered, opened or clicked)"]}]}
```
Sending the same request again returns `207` with `accepted:0, duplicates:1` and **does not change the stats**.

### `GET /campaigns/{campaign_id}/stats`
```bash
curl -s localhost:8080/campaigns/cmp_summer_sale/stats
```
After loading the seed file:
```json
{"campaign_id":"cmp_summer_sale","sent":40,"delivered":35,"opened":28,"clicked":12,
 "unique_opened":20,"unique_clicked":12,"last_event_at":"2026-08-10T18:28:00Z"}
```
- `opened`/`clicked` count distinct events; `unique_*` count distinct contacts.
- `last_event_at` = newest provider timestamp seen (tells the dashboard how fresh the data is); `null` if none.
- A campaign with no events returns `200` with all zeros.

### `GET /campaigns/{campaign_id}/events` (optional)
Newest first, keyset pagination. Query: `limit` (1–200, default 50), `cursor` (from the previous page's `next_cursor`).
```bash
curl -s 'localhost:8080/campaigns/cmp_summer_sale/events?limit=2'
curl -s 'localhost:8080/campaigns/cmp_summer_sale/events?limit=2&cursor=<next_cursor>'
```
Response: `{"campaign_id": "...", "events": [ ... ], "next_cursor": "..."}`; `next_cursor` is absent on the last page.

## Try it with the seed file
```bash
# Terminal 1
cd service
go run .

# Terminal 2
cd campaign-events/service

curl -s -X POST localhost:8080/events \
  -H 'Content-Type: application/json' \
  --data-binary @seed/events.json

curl -s localhost:8080/campaigns/cmp_summer_sale/stats
curl -s localhost:8080/campaigns/cmp_welcome/stats
curl -s localhost:8080/campaigns/cmp_winback/stats
```
First load of the seed: `received 235, accepted 211, duplicates 15, conflicts 3, rejected 6`.

## Run the tests
```bash
cd service
gofmt -l .            # should print nothing
go vet ./...
go test ./...
go test -race -count=1 ./...     # includes the concurrency tests; the one that matters
```

## Debugging exercise
```bash
cd debugging
go run -race . events.jsonl > /dev/null          # no "DATA RACE" output expected
go run . events.jsonl | diff - expected_output.txt   # empty = identical
```
Details in `BUGS.md`.
