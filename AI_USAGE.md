# AI_USAGE.md

> **Draft — edit before submitting.** Everything below is true of how this repository
> was produced, but the parts marked **[YOU]** can only be true for you after you have
> run and read the code. Replace or delete anything that does not match your experience;
> the interview will ask you to explain the code, so every line here must be something you can defend.

## Tools used
- **Claude (claude.ai chat)** — the only AI tool used for this submission.
- ChatGPT (OpenAI)

## What I used it for
- Reading the assignment and the starter files and finding the four bugs in `debugging/main.go`.
- Drafting the Go service (`service/`), its tests, and the documents (`NOTES.md`, `BUGS.md`, `README.md`).
- Computing, with a throwaway script, the numbers I expected from the seed file under my validation
  rules, so the seed test asserts exact values rather than just "doesn't crash".
- Checking the debugging fixes by porting the program's logic to Python and diffing against `expected_output.txt`.

## Important limitation

Claude's sandbox had no Go toolchain and no network, so the Go code was not compiled or executed by Claude. I installed Go locally and independently verified the implementation on November 3, 2026.

- `go test ./...` — Passed.
- `go test -race -count=1 ./...` — Passed.
- `go vet ./...` — Passed.
- `go run -race . events.jsonl` — Completed without a DATA RACE warning.
- Debugging output matched `expected_output.txt` on two observed runs.
## One AI suggestion I rejected or changed, and why

I reviewed the design choices and kept the partial-success approach. Valid events are stored even when other events in the batch are invalid, and the API returns HTTP 207. This prevents one malformed event from blocking valid events.

## One thing AI helped me understand

I understood why concurrent counter updates can cause incorrect results. An operation such as `cs.Sent++` is a read-modify-write operation. Multiple goroutines updating the same counter without synchronization can cause a data race and lost increments. A mutex protects these updates. I also understood why timestamps should be grouped using UTC rather than local time to keep daily statistics consistent across time zones.

## Anything AI generated that I had to debug

I independently verified the generated Go implementation. The unit tests, race-enabled tests, and `go vet` completed successfully. I ran gofmt on the project. No test failures or vet errors were encountered during verification.
