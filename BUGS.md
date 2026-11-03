# BUGS.md — debugging challenge (`debugging/main.go`)

The program reads `events.jsonl` and prints per-campaign stats. The spec
(`debugging/README.md`) has four rules; each bug breaks one of them. All fixes
are small additions/one-line changes — the structure of the program is untouched
(`diff -u` is ~20 changed lines).

## How I verified (read this first)

1. **Logic check against `expected_output.txt`.** I ported the program's logic to
   a small Python script with one switch per bug and diffed its output against
   `expected_output.txt`:

   | Bugs fixed            | Result vs `expected_output.txt`       |
   |-----------------------|---------------------------------------|
   | none                  | 45 lines differ                       |
   | 1 + 2 + 3             | **exact match**                       |
   | 2 + 3 (bug 1 left)    | 39 lines differ                       |
   | 1 + 3 (bug 2 left)    | 5 lines differ                        |
   | 1 + 2 (bug 3 left, machine in IST) | 36 lines differ          |

   So each of bugs 1–3 is *necessary*, and together they are *sufficient* for the
   exact expected output.
2. **Bug 4 (the race)** does not change the numbers on every run, so it cannot be
   verified by comparing output once. It is verified by the race detector:
   `go run -race . events.jsonl` must print no `WARNING: DATA RACE`, and
   `go run . events.jsonl | diff - expected_output.txt` must be empty on repeated runs.

> Status: Python logic check completed.
>  `go run -race . events.jsonl` → Completed successfully; no DATA RACE reported.
>  Diff vs expected output → Exact match on two observed runs (no differences).
## Bug 1 — Duplicates are only removed *within one batch*

- **What:** Rule 1 says each `event_id` is counted at most once across the **whole
  file**. The program only de-duplicates inside a batch of 200 lines.
- **Why:** `processBatch` creates `seen := make(map[string]bool)` on every call.
  That map is thrown away after each batch, so a retry that lands in a *later*
  batch (the file has 580 duplicated ids) looks new and is counted again.
- **Fix:** Replace the per-call map with a package-level `seenEvents` map that
  lives for the whole run (`seenEvents[ev.EventID]` instead of `seen[...]`).
  `processBatch` runs on the main goroutine only, so no lock is needed for it.
- **Verified:** Python port — leaving this bug in gives 39 differing lines; fixing
  it removes them.

## Bug 2 — `unique_opens` is keyed by contact only, not by campaign + contact

- **What:** Rule 2 says a contact who opens in two campaigns counts once *in each*.
  The program counts that contact once in total, so the second campaign is
  under-counted (e.g. `cmp_A unique_opens` came out as 549 instead of 942).
- **Why:** `openedBy` is a single global map keyed by `ev.ContactID`. The first
  campaign to see a contact "uses up" that contact for every other campaign.
- **Fix:** Key it by `ev.CampaignID + "|" + ev.ContactID`.
- **Verified:** Python port — leaving this bug in gives 5 differing lines (the
  `unique_opens` values); fixing it removes them.

## Bug 3 — Daily buckets use the machine's local time zone, not UTC

- **What:** Rule 3 says buckets use the event's UTC date. The program formats the
  date in the *local* zone of whatever machine runs it.
- **Why:** `ev.Timestamp.Local().Format("2006-01-02")`. `Local()` converts the UTC
  instant to the computer's time zone, so late-evening UTC events fall on the next
  calendar day on an Indian machine (UTC+5:30). On a UTC machine (CI, most
  servers) the bug is invisible, which is exactly why it is dangerous.
- **Fix:** `.Local()` → `.UTC()`.
- **Verified:** Python port with the machine zone set to IST: leaving it in gives
  36 differing lines; fixing it removes them. Fix is correct on any machine
  because `UTC()` does not depend on the machine's zone.

## Bug 4 — Data race on the counters (non-deterministic output)

- **What:** Rule 4 says output must be identical on every run. Eight worker
  goroutines call `apply()`, and every one does `cs.Sent++` / `cs.Delivered++` / …
  on the *same* `*CampaignStats` with no synchronisation.
- **Why:** `x++` is read-modify-write, not atomic. Two goroutines can read the same
  old value and both write old+1, so one increment is lost. How many are lost
  depends on scheduling, so totals can differ from run to run (or look right on
  one machine). The comment above `processBatch` ("the increments themselves are
  cheap") hides the problem; the earlier comment "the map itself is never written
  concurrently" is true but irrelevant — the *values* behind the pointers are.
- **Fix:** Add `var mu sync.Mutex` and take it in `apply()` (`mu.Lock(); defer mu.Unlock()`).
  Minimal and obviously correct. Trade-off: the worker pool no longer runs the
  increments in parallel, so it buys nothing here; I kept the structure because the
  task says not to restructure. Removing the pool (or per-worker partial counts that
  are merged at the end) would be the cleaner redesign.
- **Verified:** By reading, and by the race detector: run
  `go run -race . events.jsonl` (the original reports `WARNING: DATA RACE` in
  `apply`; the fixed version should report nothing), then run
  `go run . events.jsonl | diff - expected_output.txt` several times.

## Commands

```bash
cd debugging
gofmt -l .                                   # prints nothing if formatted
go run -race . events.jsonl > /dev/null      # expect: no "DATA RACE" output
for i in 1 2 3 4 5; do go run . events.jsonl | diff - expected_output.txt && echo "run $i: identical"; done
```
