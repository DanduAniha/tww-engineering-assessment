# NOTES.md

## Part 1 — Reading the brief before writing code

### My interpretation
Relay hands messages to delivery providers. Providers call our webhook as things
happen to each message (sent → delivered → opened → clicked). I own two things:
**(1)** a webhook that accepts batches of provider events safely, and **(2)** a read
API that gives a dashboard per-campaign numbers. The hard parts are not the HTTP: they
are that providers *retry* (so ingestion must be idempotent) and events arrive *late and
out of order* (so numbers must not depend on arrival order). Correctness of the numbers
matters more than features.

### Assumptions
- `event_id` is unique per event across all providers/campaigns, so it is the identity
  of an event. "Same event" = same `event_id`.
- If the same `event_id` arrives again with the **same** campaign, contact, type and
  timestamp it is a retry (**duplicate**): ignored, not an error. If it arrives with
  **different** content it is a **conflict**: the first one wins, the second is reported
  and ignored (we cannot know which is right; silently overwriting would make counts
  move after the fact).
- `metadata` is optional and informational; it is not part of event identity and does not
  affect counts.
- Counts are per **event**, not per message: `opened` = number of distinct accepted
  `opened` events. I also return `unique_opened` / `unique_clicked` = distinct contacts,
  because "how many opened" is ambiguous (see below) and a marketer usually means the second.
- The funnel is **not enforced**: an `opened` is counted even if we never saw that
  contact's `delivered` (providers drop and reorder events; rejecting would under-count).
- `timestamp` must be RFC 3339 (`2026-08-10T09:15:04Z`; offsets accepted, stored in UTC).
  Late events are fine (any past timestamp). Timestamps more than 24h in the **future**
  are rejected (clock-skew tolerance; configurable via `-max-future-skew`, `0` disables).
- Normalisation: surrounding whitespace is trimmed from ids; `type` is lower-cased
  (`OPENED` in the seed becomes `opened`). Unknown types (`spam_report`) are **rejected**
  rather than counted or silently dropped — we report them so they are visible.
- A campaign we have never heard of is not an error for the stats endpoint: it returns
  zeros with HTTP 200 (a brand-new campaign legitimately has no events yet).
- In-memory storage is acceptable (explicitly allowed); data is lost on restart.

### Ambiguities I noticed in the brief
- What "the same event" means (event_id only? event_id + content?).
- What "opened" counts: events, or people? Does an opened-before-delivered count?
- What to return for duplicates / invalid events / mixed batches (and how that interacts
  with provider retry behaviour).
- Whether `delivered ≤ sent`, `opened ≤ delivered` should hold or be enforced.
- Does the dashboard need time ranges (today / last hour) or only all-time totals?
- "Live dashboard" — how fresh must numbers be?
- Do events ever need to be corrected or deleted (e.g. a provider retracting a bot open)?
- Is a campaign created before events arrive, or implied by the first event?
- What do we do with event types beyond the four (`bounced`, `spam_report`, `unsubscribed`)?

### Questions I would ask the PM
1. Should "opened" on the dashboard be total opens or unique people? (I return both.)
2. Should the dashboard show time series (per hour/day), not just totals?
3. Is it OK to ignore unknown event types, or do you want bounces/spam reports tracked too?
4. What do providers do on a non-2xx response — retry forever, give up after N tries, or
   treat 4xx as permanent? (This decides my status codes for bad events.)
5. Can an `event_id` ever be reused by a provider, or collide between two providers?
6. Do you want a conflicting duplicate to overwrite, be ignored, or page someone?
7. Do you count machine opens (e.g. mail-privacy proxies) as opens?
8. What retention do we need — do we keep raw events, or only counters?

### Decisions and why
| Question | Decision | Why |
|---|---|---|
| Identity of an event | `event_id` | It is what providers retry with; content can't identify an event because two real events can look alike. |
| Duplicate | `200`-class, ignored, counted in `duplicates` | Provider must stop retrying; counters untouched. |
| Same id, different content | ignored, listed in `issues` as `conflict` | First write wins keeps counters stable; visible so it can be investigated. |
| Invalid event | rejected, listed in `issues` with reasons | Never silently dropped; never blocks other events. |
| Mixed batch | valid events are stored; `207` | Providers retry on non-2xx; retrying cannot fix a bad event, and good events are already saved. |
| Nothing usable | `422` | Readable array but zero usable events. |
| Not a JSON array / broken JSON | `400` | Nothing can be recovered from a syntactically broken body. |
| Storage | in-memory maps under one `sync.RWMutex` | Simple, correct, explainable; counters updated in the same critical section as the dedup check. |

### What I prioritised (in order)
1. This document (assumptions first).
2. `POST /events` with per-element validation and idempotency; `GET .../stats`.
3. Concurrency safety (one lock, batch applied atomically) + tests for the behaviour I'd
   be most embarrassed to break: duplicates, mixed batches, concurrent submits, seed file.
4. Debugging challenge → `BUGS.md`.
5. Scale memo and angry-marketer write-ups (below).
6. Optional `GET /campaigns/{id}/events` (done last, keyset pagination).


## Part 4 — Scale memo: 100 million events/day

100M/day is ~1,150 events/s on average and maybe 10× that in bursts (campaigns send in waves).

**What breaks first, specifically**
- **Memory.** I keep every event forever, plus a map of every `event_id`. At ~200–300 bytes
  per event that is tens of GB per day. This breaks before CPU does.
- **Durability.** A restart or deploy wipes all counters. Fine for a prototype, fatal at scale.
- **The single lock.** Every POST takes one global write lock, so ingest is effectively
  single-threaded, and every dashboard read queues behind writers.
- **Sorted-slice insert** (used for the events list) is O(n) per insert; hot campaigns get slow.
- It is also a single node: no failover.

**What I would change, in order**
1. Move to a durable store where **the database enforces uniqueness**: a table of events
   with `PRIMARY KEY (event_id)` (or `(provider, event_id)`), inserted with
   `ON CONFLICT DO NOTHING`. Dedup stops being "a map in RAM" and becomes a constraint that survives
   restarts and works across many instances.
2. Keep counters as **rollups updated in the same transaction as the insert, only when a row
   was actually inserted** (`campaign_id, type, hour` buckets). Dashboard reads then read a few rows, not
   all events.
3. Shard by `campaign_id` so one hot campaign doesn't lock out the rest.
4. Only then add a queue (below) and read replicas/caching for the dashboard.
5. For unique-contact counts at that size, consider approximate counting (HyperLogLog) —
   exact distinct counts per campaign get expensive.

**Would the API contract change?** The provider contract (payload) cannot change. `POST /events`
would probably return `202 Accepted` once the queue has it, since "accepted" no longer means
"already counted". The stats endpoint keeps its shape but gains an explicit freshness field
(`as_of`) because numbers become eventually consistent. **Storage design: yes, completely** (above).

**Would I introduce a queue?** Yes, to absorb bursts and decouple "acknowledge the provider fast"
from "update the database" (providers retry on slow responses, which creates *more* load). Receiver
validates, writes to e.g. Kafka partitioned by `campaign_id`, returns 2xx. New problems it creates:
at-least-once delivery (consumers must be idempotent), consumer lag (stale dashboards), poison messages
(need a dead-letter queue), ordering only within a partition, back-pressure, and a new thing to operate
and monitor.

**Where duplicates can now sneak in that couldn't before**
- Queue redelivery (consumer crashes after the DB write but before committing its offset).
- Several consumers/instances, each with its own idea of "seen" — a RAM map no longer works.
- Provider retries that hit different receiver instances at the same moment.
- Replays/backfills when we reprocess a topic.
- `event_id` collisions between providers (key must include the provider).
The defence is the same everywhere: the database unique constraint is the single source of truth.

**Keeping counters trustworthy**
- Counters only change as a side effect of a *successful insert* of a new event.
- Periodically recompute counts from raw events and compare with the rollups; alert on drift.
- Never "fix" a counter by hand; fix the events and recompute.
- Show `as_of`, so a lagging number is labelled, not mistaken for a wrong one.

**What I would monitor**
Requests/s and 2xx/4xx/5xx rate; p50/p99 latency of `POST /events`; queue depth and consumer lag; dead-letter
size; duplicate ratio and rejection ratio per provider and per reason (a spike means a provider changed something);
lag between event `timestamp` and `received_at` (late-arrival profile); DB write latency and lock waits;
rollup-vs-raw drift; memory/disk growth.

**What I would deliberately not solve yet**
Exactly-once processing (idempotent at-least-once is enough), multi-region, exact distinct counts at scale, long-term
retention/archival, per-contact journey analytics, authentication/rate-limiting per provider. I would first measure real
traffic shape — "I don't know the burst factor, so my first step would be to measure it from a week of logs."

---

## Part 5 — The angry marketer

10:00 → 10:30: Sent 1,000,000 → 1,000,000; Delivered 970,000 → 975,000 (up); Opened 250,000 → 248,000 (**down**); Clicked 20,000 → 20,000.

### Is "delivered went up" suspicious?
No. Sent is flat at 1,000,000, so the campaign finished sending and the remaining ~30,000 messages are
still resolving: deliveries trickle in (retries, slow receiving servers, late events). 975,000 ≤ 1,000,000 so
the invariant `delivered ≤ sent` holds. Delivered rising while sent is flat is exactly what late events look like. It would
be suspicious if delivered exceeded sent, or jumped far past the growth rate seen earlier.

### The real question: why did opened go *down*?
In my design counters only ever increase (every accepted event adds; duplicates and rejections change nothing).
So a decrease means something *other than ingestion* is happening. Plausible explanations, roughly most-to-least likely:

**Expected behaviour (not a bug)**
1. The provider **reclassified opens** — e.g. removed machine/proxy opens or bot opens and sent corrections.
2. The dashboard is a **rolling window** ("last 24h"): old opens fell out of the window while new ones haven't replaced them.
3. **Definition changed** between the two views (total opens → unique opens, or a bot filter was switched on).
4. A **reconciliation/dedup job** ran and removed previously double-counted opens (a *correction*, even if surprising).
5. **Contacts were deleted/suppressed** (privacy request, unsubscribe purge) and their events removed.
6. **Retention/TTL** deleted old events.
7. **Events were re-attributed** to a different campaign (campaign merged/split).
8. The numbers are **approximate by design** (sampling, HyperLogLog) and have noise.

**Bugs**
9. **Different instances/caches**: two app servers (or a cache and the DB) with different state; the dashboard hit a lagging one at 10:30.
   (With my in-memory design this is *certain* to happen the moment there are two instances.)
10. **Restart/deploy/failover** replayed only part of the data (partial restore from snapshot or queue).
11. A **rollup recompute overwrote** the live counter with a smaller value computed from a source that hadn't received the late events yet.
12. A **dedup bug** that collapses distinct events (e.g. a too-coarse key), or a **conflict** rule that dropped legitimate events.
13. **Race/lost update** in counter code (can lose increments; can look like a drop if a recompute then replaces the number).
14. **Query inconsistency**: `sent`/`delivered`/`opened` read from different tables or at different moments (non-atomic read).
15. **Time zone / window boundary** bug in the dashboard query.
16. The dashboard **client** caching/rounding or showing a stale widget.

### What I would check first, and how I'd decide bug vs expected
1. **Is the raw event set shrinking?** Count `opened` events by `received_at ≤ 10:00` vs `≤ 10:30`. If raw events are append-only, the
   store did not lose data → the cause is in the read path or a derived number (go to 2). If the raw set itself shrank → deletion/TTL/restore (5, 6, 10).
2. **Does the same query give the same answer twice, from every instance?** Different answers → instance/cache problem (9). Same answer → deterministic.
3. **What changed between 10:00 and 10:30?** Deploys, restarts, config flags, reconciliation/cron jobs, provider announcements about bot/proxy filtering.
4. **Which events "disappeared"?** If I have a snapshot from each time, diff the `event_id` sets and look at what they share (one provider? one email client? one hour?).
   A single shared cause points to a rule/reclassification; random loss points to a bug.
**Decision rule:** it is a **bug** if a number decreases with no documented rule that allows it, if the stored events can't explain the number, or if the same question gives different answers.
It is **expected** if I can point to a rule (window, provider correction, definition) that explains every missing event, and if recomputing from raw events reproduces the dashboard value.
Either way the product fix is the same: make the rule visible (`as_of`, window label, "corrected by provider") so a marketer doesn't need a ticket to discover it.

---

## What I completed / what I intentionally skipped

**Completed**
- `POST /events`: per-element validation, normalisation, idempotent dedup by `event_id`, conflict detection, mixed-batch handling, meaningful status codes (200/207/422/400/413).
- `GET /campaigns/{id}/stats`: sent/delivered/opened/clicked + unique_opened/unique_clicked + last_event_at; unknown campaign → zeros.
- Optional `GET /campaigns/{id}/events` with keyset (cursor) pagination, newest first.
- Concurrency safety: one `RWMutex`; a batch is applied atomically; tests designed for `-race`.
- Tests written (`go test ./...`, results in the verification log below): duplicates, mixed batches, 400/422 cases, out-of-order/late events, unique vs total opens, conflicts, timestamp rules, pagination, concurrent submits, and the seed file with exact expected numbers.
- Debugging challenge: 4 bugs found and fixed minimally (`BUGS.md`).
- README, this document, AI_USAGE.md.

Verification log

- `gofmt -w .` → Executed successfully
- `go vet ./...` → Passed
- `go test -race -count=1 ./...` → Passed
- Debugging: `go run -race . events.jsonl` → Completed without race reports
- Diff vs expected output → Exact match (no differences)
- POST /events → Passed
- GET /campaigns/{campaign_id}/stats → Passed
- Duplicate event test → Passed; duplicate correctly ignored and statistics unchanged.

**Intentionally skipped, and why**
- Persistence (SQLite): allowed to skip; the scale memo explains what I'd do instead.
- Time-series / date-range stats: the brief only asked for totals; would ask the PM first (question 2).
- Authentication, Docker, deployment, frontend: explicitly out of scope.
- Counting `bounced`/`spam_report`: unknown types are rejected and reported until the PM says otherwise.
- Memory bounds on stored events: unbounded in this version (stated in the scale memo as the first thing to break).
- Graceful shutdown, metrics endpoint, structured logging.

## If I had another day
1. Replace the in-memory store with SQLite/Postgres: `event_id` primary key, rollup table updated in the same transaction, then delete the RAM maps.
2. Hourly time buckets so the dashboard can chart activity and I can answer the "opened went down" question from data instead of guessing.
3. A reconciliation command that recomputes counters from raw events and diffs them against the live counters.
4. Per-provider identity in the dedup key and basic per-provider rejection metrics.
5. Load test the ingest path to measure the real lock contention before choosing how to shard.
