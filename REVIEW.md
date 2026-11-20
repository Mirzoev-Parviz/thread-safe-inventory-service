# Race Condition Review: `InventoryService`

The original `InventoryService` has no synchronization at all. Every method reads
and writes the shared `products` map and the `Stock` field of `*Product` values
from whatever goroutine calls it. Below are the four race conditions, followed by
non-concurrency bugs found along the way.

Fixed implementation: [`inventory.go`](inventory.go). Tests: [`inventory_test.go`](inventory_test.go).

---

## Race Condition 1: `GetStock` — unsynchronized read

- **Code:**
  ```go
  product := s.products[productID]
  ...
  return product.Stock
  ```
- **What happens:** `GetStock` reads `product.Stock` while `Reserve` /
  `ReserveMultiple` / `SafeReserve` may be writing the same field in another
  goroutine. This is a data race as defined by the Go memory model: there is no
  happens-before relation between the write and the read, so the result is
  undefined. In practice the reader can see a stale value, a value the program
  never logically held (from a half-done multi-item reservation), or a value
  cached in a register that never updates. If the service ever gains a way to
  add or remove products, the unsynchronized map read also becomes
  `fatal error: concurrent map read and map write`, which crashes the whole
  process and cannot be recovered.
- **Production scenario:** a storefront handler (goroutine G1) calls
  `GetStock("sku-1")` to render "3 left in stock" while a checkout handler
  (goroutine G2) runs `Reserve("sku-1", 3)`. G1 may show "3 left" after the
  items are already gone, or, during a `ReserveMultiple`, observe product A
  already decremented and product B not yet, an inconsistent snapshot. A
  `go test -race` run of this pair reports `WARNING: DATA RACE`.
- **Fix approach:** guard all reads with `mu.RLock()` / `mu.RUnlock()` on a
  `sync.RWMutex` shared by the whole service. Read locks don't block each
  other, so read-heavy traffic stays cheap, but they do wait for any in-progress
  writer, so a reader never sees a half-applied update.

---

## Race Condition 2: `Reserve` — check-then-act (TOCTOU) and lost update

- **Code:**
  ```go
  if product.Stock < quantity {   // check
      return ErrInsufficientStock
  }
  product.Stock -= quantity       // act (read-modify-write)
  ```
- **What happens:** there are two separate problems:
  1. *Check-then-act.* The check and the update are not atomic. Several
     goroutines can all pass the check against the same old value before any
     of them decrements, so all of them "succeed".
  2. *Lost update.* `product.Stock -= quantity` is a read, a subtract and a
     write. Two goroutines can both read `10`, both write `9`, and two units
     are sold while only one is deducted.

  Together these cause **overselling**. Either the stock goes negative, or it
  stays non-negative while more units were sold than existed, which is worse
  because nothing looks wrong.
- **Production scenario:** a product has `Stock = 1`. Goroutines G1 and G2
  (two customers clicking "Buy" at the same moment) both execute
  `product.Stock < 1` → false, both pass, both execute `Stock -= 1`. Result:
  two orders confirmed for one physical item, `Stock = -1` (or `0` if the
  decrement was lost). In `TestReserve_ConcurrentOversell`, an unlocked version
  produced 134 successful reservations out of 100 units and a final stock of −33.
- **Fix approach:** take the write lock (`mu.Lock()`) *before* the check and
  release it only *after* the update (`defer mu.Unlock()`), so check and update
  form one critical section. Any other `Reserve` must wait and then re-checks
  against the already-decremented value.

---

## Race Condition 3: `ReserveMultiple` — check-all-then-reserve-all is not atomic

- **Code:**
  ```go
  // Check all first
  for _, item := range items {
      product := s.products[item.ProductID]
      if product.Stock < item.Quantity { return ErrInsufficientStock }
  }
  // Then reserve all
  for _, item := range items {
      s.products[item.ProductID].Stock -= item.Quantity
  }
  ```
- **What happens:** the two-phase structure looks all-or-nothing, but with no
  lock the gap between phase 1 and phase 2 is wide open. Any number of other
  goroutines can reserve the same products after this call has validated them
  and before it deducts. Phase 2 then subtracts from stock that no longer
  exists. The decrements in phase 2 are also unsynchronized read-modify-writes
  (the lost update from #2), and other goroutines can observe the basket
  half-applied, with A already decremented and B not yet.
- **Production scenario:** stock is `A=1, B=1`. Goroutine G1 reserves basket
  `{A:1, B:1}`, goroutine G2 reserves `{B:1}`. G1 finishes phase 1 (A and B
  both available). G2 runs `Reserve("B", 1)` and takes the last B. G1 runs phase
  2: `A=0, B=-1`. G1's order is confirmed though B was never available to it.
  The basket-level guarantee is gone. In the test suite, an unlocked version of
  `TestReserveMultiple_ConcurrentAllOrNothing` ended with 32 successful baskets
  for 30 units of B, and `B = -1`.
- **Fix approach:** hold the service-wide write lock across *both* phases.
  Phase 1 only reads, so an early return leaves the state untouched. Phase 2
  cannot fail, so either everything is applied or nothing is. A single lock
  for the whole service also avoids the A→B / B→A deadlock that per-product
  locks would create (see ANSWERS.md, Q2).

---

## Race Condition 4: `SafeReserve` — the mutex is local to the call

- **Code:**
  ```go
  func (s *InventoryService) SafeReserve(productID string, quantity int) error {
      var mu sync.Mutex   // new mutex on every call
      mu.Lock()
      defer mu.Unlock()
      ...
  }
  ```
- **What happens:** `mu` is a local variable, so every call to `SafeReserve`
  creates its own fresh, unlocked mutex. Mutual exclusion only works between
  goroutines that lock the *same* mutex. Here no two goroutines ever share one:
  each `Lock()` always succeeds at once, and the body runs with exactly the
  same check-then-act and lost-update races as `Reserve` (#2). It also gives no
  protection against `Reserve`, `ReserveMultiple` or `GetStock`, which don't
  touch this mutex at all. The code looks thread-safe and is not, which is
  arguably worse than an obvious unlocked function, because reviewers trust it.
- **Production scenario:** a team "fixes" an oversell incident by switching
  the checkout handler to `SafeReserve`. On the next flash sale, 500 goroutines
  call `SafeReserve("sku-1", 1)` for 100 units. Each one locks its own private
  mutex, all of them pass the stock check, and the product oversells exactly as
  before, while the incident is marked resolved.
- **Fix approach:** the mutex must be shared state with the same lifetime as
  the data it protects. Make it a field of the service (`mu sync.RWMutex` in
  `SafeInventoryService`), and use it from every method that touches
  `products`. Never copy the service struct after first use (`go vet`'s
  `copylocks` check catches this), and always use it through a pointer.

---

## Other bugs found during review (not races, also fixed)

| Location | Bug | Fix in `SafeInventoryService` |
|---|---|---|
| `ReserveMultiple`, `SafeReserve` | No `nil` check: an unknown product ID causes a nil-pointer **panic**, which crashes the request (or the process if not recovered). | Returns `ErrProductNotFound`. |
| `ReserveMultiple` | **Duplicate product IDs** are checked independently: `{A:6}, {A:6}` with `Stock=10` passes phase 1 (6 ≤ 10 twice), then leaves `Stock=-2`. | Quantities are summed per product before checking. |
| `Reserve`, `ReserveMultiple`, `SafeReserve` | **Zero or negative quantities** are accepted. `Reserve(id, -100)` adds 100 units of stock out of nowhere. | Returns `ErrInvalidQuantity` for `quantity <= 0`. |
| `ReserveMultiple` | When it fails, the error does not say which product failed. | Errors are wrapped with the product ID and amounts, and still match `errors.Is(err, ErrInsufficientStock)`. |
