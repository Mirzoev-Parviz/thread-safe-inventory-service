# Answers

## Q1: Why is a mutex declared inside `SafeReserve` completely useless?

```go
func (s *InventoryService) SafeReserve(...) error {
    var mu sync.Mutex  // <-- HERE
    mu.Lock()
    ...
}
```

A mutex gives mutual exclusion only between goroutines that lock **the same
mutex instance**. Its state (locked / unlocked, the queue of waiters) lives in
the `sync.Mutex` value itself.

`var mu sync.Mutex` inside the function creates a **new, zero-value (unlocked)
mutex on every call**. It is a local variable, scoped to that one invocation:

- Goroutine G1 calls `SafeReserve` and gets mutex `m1`. `m1.Lock()` succeeds.
- Goroutine G2 calls `SafeReserve` at the same time and gets a different mutex
  `m2`. `m2.Lock()` also succeeds at once, because nobody has ever locked `m2`.
- Both goroutines are now inside the "critical section" at once, reading and
  writing the same `product.Stock`.

So `Lock()` never blocks and never orders anything. No goroutine can ever
contend on another call's mutex, so there is no happens-before edge between
calls. The race detector still reports the data race on `product.Stock`, and the
check-then-act oversell is exactly as likely as with no mutex at all. The mutex
also cannot protect against the other methods (`Reserve`, `ReserveMultiple`,
`GetStock`), since they never see it.

The fix is to make the mutex shared state that lives as long as the data it
guards: a field of the service struct, used by every method through a pointer
receiver (`SafeInventoryService.mu`).

---

## Q2: Per-product locks, G1 reserves A then B, G2 reserves B then A

This is the classic **lock-ordering deadlock** (an ABBA deadlock):

```
G1: lock(A) ── ok ──► lock(B) ── waits for G2 ...
G2: lock(B) ── ok ──► lock(A) ── waits for G1 ...
```

Each goroutine holds one lock and waits forever for the lock the other holds.
All four Coffman conditions hold: mutual exclusion, hold-and-wait, no
preemption, circular wait. Neither goroutine can proceed. If every goroutine
in the program ends up blocked, the runtime aborts with
`fatal error: all goroutines are asleep - deadlock!`. In a real server other
goroutines keep running, so there is no crash: the two requests just hang,
those products' locks stay held, and every later request touching A or B
queues up behind them. That is a silent, growing outage.

**How to prevent it:**

1. **Global lock ordering (the standard fix for per-product locks).** Always
   acquire locks in one fixed order, e.g. sort the item IDs and lock in
   ascending order. Both G1 and G2 then lock A first, then B, so the circular
   wait cannot form. Also deduplicate IDs first, because `sync.Mutex` is not
   reentrant and locking A twice deadlocks the goroutine against itself. Release
   all locks (in any order) after the whole basket is checked and applied, to
   keep all-or-nothing semantics.
2. **One coarse lock for the whole operation** (what this implementation does).
   `ReserveMultiple` takes the single service-wide `mu.Lock()`, so only one
   lock exists and a cycle is impossible. The cost is lower parallelism:
   unrelated products can't be reserved at the same time. That is fine here
   because the critical section is a few map lookups and integer operations.
   Switch to option 1 only if profiling shows contention on `mu`.
3. **Try-lock with back-off** (`TryLock`: if the second lock fails, release the
   first and retry). This works but risks livelock and adds complexity, so it
   is rarely the right choice in Go.

The test `TestReserveMultiple_ConcurrentAllOrNothing` deliberately alternates
`[A, B]` and `[B, A]` item order across 200 goroutines to cover this scenario.

---

## Q3: Why is the "release early" fix worse than no locks?

```go
s.mu.Lock()
product := s.products[productID]
s.mu.Unlock()  // Release early

if product.Stock < quantity {      // (1) unlocked read
    return ErrInsufficientStock
}

s.mu.Lock()
product.Stock -= quantity          // (2) locked write
s.mu.Unlock()
```

**The specific bug:** the check and the update sit in two **separate critical
sections** with an unprotected gap between them. That is a time-of-check /
time-of-use (TOCTOU) race. The lock protects the map lookup, but the code then
keeps using the `*Product` pointer after releasing the lock, and the check in
(1) reads `product.Stock` with no lock at all. Two goroutines can both pass (1)
with `Stock = 1`, then take turns at (2), and both succeed. **Overselling
remains.** On top of that:

- **(1) is still a data race.** It reads `Stock` while another goroutine writes
  it under the lock. A lock only helps if *both* sides take it.
- **Negative stock is now guaranteed.** Without locks, the decrement itself can
  be lost (two goroutines read 1, both write 0), which accidentally keeps
  `Stock` at 0 while extra units are sold. With the lock around (2), every
  oversold unit is subtracted, so `Stock` goes negative. Code downstream that
  relies on `Stock >= 0` (display, re-ordering, accounting) now breaks. When I
  ran 200 goroutines against 100 units 500 times, the unlocked version never
  went below 0 but confirmed up to 160 reservations, and the "fixed" version
  went down to `Stock = -20`.
- **No nil check.** `product` is dereferenced without checking it, so an
  unknown ID panics.

**Why "worse" than no locks:** it is not necessarily *more frequently* wrong.
In my measurement the unlocked version oversold more in absolute numbers. It is
worse because:

1. **It looks correct.** There are `Lock`/`Unlock` calls around every access to
   shared state, so in review it reads as "already made thread-safe". The bug is
   now hidden behind code that appears to be the fix, and the team stops looking
   for it. Code with no locks at all is obviously unsafe and gets fixed.
2. **It pays the full cost and buys nothing.** Two lock round-trips per call,
   with contention under load, and still no atomicity.
3. **It turns a silent counter error into broken data (negative stock)**, while
   the success rate of overselling stays high.

**Correct version:** one critical section from the lookup through the check to
the update. `Lock` comes before the lookup, `defer Unlock` follows it, and no
pointer to a product is used outside the lock (see `SafeInventoryService.Reserve`).

---

## Q4: `go test -race` reports nothing. Is the code race-free?

**No.** A clean `-race` run means *"no data race happened in the executions that
ran"*. It does not mean *"no race is possible"*. Reasons:

1. **The detector is dynamic, not a static proof.** It instruments memory
   accesses and reports two accesses only when they *actually occur* in a run
   without a happens-before relation. Code paths the tests never execute, or
   interleavings that never happen, are never examined. A race in an error
   path, a rarely used method, or one that needs specific timing can stay
   hidden.
2. **It depends on the test exercising concurrency.** If the tests call
   `Reserve` from a single goroutine, there is nothing to detect. Tests have to
   actually run concurrent operations, which is why the tests here start many
   goroutines at once behind a shared start channel and `sync.WaitGroup`.
3. **It only finds *data races*, not *race conditions*.** A logic race (wrong
   result from an unlucky interleaving) where every individual access is
   properly synchronized is invisible to it. The Q3 code with the check also
   moved under a lock (`Lock → read Stock → Unlock → Lock → write → Unlock`)
   has no data race at all, and `-race` is silent, yet it still oversells. Same
   for a deadlock-prone lock order (Q2), atomicity bugs across several
   `atomic` operations, or an all-or-nothing operation split across two
   critical sections. These are only caught by tests that check
   **invariants** (exact success count, final stock, unchanged state after a
   failed basket), as `TestReserve_ConcurrentOversell` and
   `TestReserveMultiple_Atomicity` do.
4. **Limited history.** The detector keeps a bounded window of past accesses
   per memory location (a few shadow words), so in rare cases even a race that
   did occur during the run can go unreported.
5. **It does not find deadlocks or leaked goroutines.** Those need other tools
   (timeouts in tests, `goleak`, pprof goroutine dumps).

What `-race` gives you is real evidence, not a proof. Use it together with
invariant-checking concurrent tests, run with `-count=N` to try more
interleavings, and with careful review of critical-section boundaries.
