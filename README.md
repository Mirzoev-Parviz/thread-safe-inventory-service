# Thread-Safe Inventory Service

A concurrent-safe inventory in Go using `sync.RWMutex`.

| File | Contents |
|---|---|
| [`inventory.go`](inventory.go) | `SafeInventoryService`: `GetStock` (read lock), atomic `Reserve`, all-or-nothing `ReserveMultiple` |
| [`inventory_test.go`](inventory_test.go) | Concurrent tests (oversell, atomicity, RWMutex semantics) and edge cases |
| [`REVIEW.md`](REVIEW.md) | The 4 race conditions in the original implementation |
| [`ANSWERS.md`](ANSWERS.md) | Answers to Q1–Q4 |

## Run

```sh
go test -race -count=1 ./...
go test -race -count=50 -run 'Concurrent' ./...   # stress more interleavings
```
