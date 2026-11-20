package inventory

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startTogether releases n goroutines at once and waits for all of them.
func startTogether(n int, fn func(i int)) {
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
}

func TestReserve_ConcurrentOversell(t *testing.T) {
	const (
		initialStock = 100
		goroutines   = 200
	)
	s := NewSafeInventoryService(Product{ID: "A", Name: "Widget", Stock: initialStock})

	var succeeded, insufficient, other atomic.Int64
	startTogether(goroutines, func(int) {
		err := s.Reserve("A", 1)
		switch {
		case err == nil:
			succeeded.Add(1)
		case errors.Is(err, ErrInsufficientStock):
			insufficient.Add(1)
		default:
			other.Add(1)
		}
	})

	if got := succeeded.Load(); got != initialStock {
		t.Errorf("succeeded = %d, want %d", got, initialStock)
	}
	if got := insufficient.Load(); got != goroutines-initialStock {
		t.Errorf("insufficient = %d, want %d", got, goroutines-initialStock)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("unexpected errors = %d, want 0", got)
	}
	if got := s.GetStock("A"); got != 0 {
		t.Errorf("final stock = %d, want 0", got)
	}
}

func TestReserveMultiple_Atomicity(t *testing.T) {
	s := NewSafeInventoryService(
		Product{ID: "A", Stock: 10},
		Product{ID: "B", Stock: 5},
	)

	err := s.ReserveMultiple([]ReserveItem{
		{ProductID: "A", Quantity: 8},
		{ProductID: "B", Quantity: 8},
	})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("err = %v, want ErrInsufficientStock", err)
	}
	if got := s.GetStock("A"); got != 10 {
		t.Errorf("stock A = %d, want 10 (unchanged)", got)
	}
	if got := s.GetStock("B"); got != 5 {
		t.Errorf("stock B = %d, want 5 (unchanged)", got)
	}
}

func TestReserveMultiple_ConcurrentAllOrNothing(t *testing.T) {
	const (
		stockA     = 50
		stockB     = 30
		goroutines = 200
	)
	s := NewSafeInventoryService(
		Product{ID: "A", Stock: stockA},
		Product{ID: "B", Stock: stockB},
	)

	var succeeded, insufficient, other atomic.Int64
	startTogether(goroutines, func(i int) {
		// alternate A→B / B→A order
		items := []ReserveItem{{ProductID: "A", Quantity: 1}, {ProductID: "B", Quantity: 1}}
		if i%2 == 1 {
			items[0], items[1] = items[1], items[0]
		}
		err := s.ReserveMultiple(items)
		switch {
		case err == nil:
			succeeded.Add(1)
		case errors.Is(err, ErrInsufficientStock):
			insufficient.Add(1)
		default:
			other.Add(1)
		}
	})

	if got := succeeded.Load(); got != stockB {
		t.Errorf("succeeded = %d, want %d", got, stockB)
	}
	if got := insufficient.Load(); got != goroutines-stockB {
		t.Errorf("insufficient = %d, want %d", got, goroutines-stockB)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("unexpected errors = %d, want 0", got)
	}
	if got := s.GetStock("A"); got != stockA-stockB {
		t.Errorf("final stock A = %d, want %d", got, stockA-stockB)
	}
	if got := s.GetStock("B"); got != 0 {
		t.Errorf("final stock B = %d, want 0", got)
	}
}

func TestGetStock_ConcurrentWithReserve(t *testing.T) {
	const initialStock = 1000
	s := NewSafeInventoryService(Product{ID: "A", Stock: initialStock})

	var (
		wg   sync.WaitGroup
		done = make(chan struct{})
	)

	const readers = 8
	errs := make(chan string, readers)
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			last := initialStock
			for {
				select {
				case <-done:
					return
				default:
				}
				got := s.GetStock("A")
				if got < 0 || got > last {
					errs <- "observed invalid stock sequence"
					return
				}
				last = got
			}
		}()
	}

	startTogether(initialStock, func(int) {
		if err := s.Reserve("A", 1); err != nil {
			t.Errorf("Reserve: %v", err)
		}
	})
	close(done)
	wg.Wait()
	close(errs)

	for e := range errs {
		t.Error(e)
	}
	if got := s.GetStock("A"); got != 0 {
		t.Errorf("final stock = %d, want 0", got)
	}
}

func TestRWMutex_ReadersDoNotBlockEachOther(t *testing.T) {
	s := NewSafeInventoryService(Product{ID: "A", Stock: 7})

	s.mu.RLock()
	defer s.mu.RUnlock()

	got := make(chan int, 1)
	go func() { got <- s.GetStock("A") }()

	select {
	case v := <-got:
		if v != 7 {
			t.Errorf("GetStock = %d, want 7", v)
		}
	case <-time.After(time.Second):
		t.Fatal("GetStock blocked while another reader held the read lock")
	}
}

func TestRWMutex_WriterIsExclusive(t *testing.T) {
	s := NewSafeInventoryService(Product{ID: "A", Stock: 7})

	s.mu.RLock()
	done := make(chan error, 1)
	go func() { done <- s.Reserve("A", 1) }()

	select {
	case <-done:
		s.mu.RUnlock()
		t.Fatal("Reserve completed while a read lock was held")
	case <-time.After(50 * time.Millisecond):
	}

	s.mu.RUnlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Reserve did not proceed after the read lock was released")
	}
	if got := s.GetStock("A"); got != 6 {
		t.Errorf("stock = %d, want 6", got)
	}
}

func TestReserve_Errors(t *testing.T) {
	tests := []struct {
		name      string
		productID string
		quantity  int
		wantErr   error
		wantStock int
	}{
		{"ok", "A", 3, nil, 2},
		{"exact stock", "A", 5, nil, 0},
		{"insufficient", "A", 6, ErrInsufficientStock, 5},
		{"unknown product", "X", 1, ErrProductNotFound, 5},
		{"zero quantity", "A", 0, ErrInvalidQuantity, 5},
		{"negative quantity", "A", -10, ErrInvalidQuantity, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSafeInventoryService(Product{ID: "A", Stock: 5})
			err := s.Reserve(tt.productID, tt.quantity)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := s.GetStock("A"); got != tt.wantStock {
				t.Errorf("stock = %d, want %d", got, tt.wantStock)
			}
		})
	}
}

func TestReserveMultiple_Errors(t *testing.T) {
	tests := []struct {
		name         string
		items        []ReserveItem
		wantErr      error
		wantA, wantB int
	}{
		{"ok", []ReserveItem{{"A", 4}, {"B", 2}}, nil, 6, 3},
		{"empty", nil, nil, 10, 5},
		{"unknown product rolls back nothing", []ReserveItem{{"A", 1}, {"X", 1}}, ErrProductNotFound, 10, 5},
		{"duplicate ids are summed", []ReserveItem{{"A", 6}, {"A", 6}}, ErrInsufficientStock, 10, 5},
		{"duplicate ids within stock", []ReserveItem{{"A", 4}, {"A", 6}}, nil, 0, 5},
		{"invalid quantity", []ReserveItem{{"A", 1}, {"B", 0}}, ErrInvalidQuantity, 10, 5},
		{"negative quantity", []ReserveItem{{"A", -5}}, ErrInvalidQuantity, 10, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewSafeInventoryService(
				Product{ID: "A", Stock: 10},
				Product{ID: "B", Stock: 5},
			)
			err := s.ReserveMultiple(tt.items)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got := s.GetStock("A"); got != tt.wantA {
				t.Errorf("stock A = %d, want %d", got, tt.wantA)
			}
			if got := s.GetStock("B"); got != tt.wantB {
				t.Errorf("stock B = %d, want %d", got, tt.wantB)
			}
		})
	}
}

func TestGetStock_UnknownProduct(t *testing.T) {
	s := NewSafeInventoryService()
	if got := s.GetStock("missing"); got != 0 {
		t.Errorf("GetStock = %d, want 0", got)
	}
}

func TestNewSafeInventoryService_CopiesInput(t *testing.T) {
	products := []Product{{ID: "A", Stock: 5}}
	s := NewSafeInventoryService(products...)
	products[0].Stock = 999
	if got := s.GetStock("A"); got != 5 {
		t.Errorf("GetStock = %d, want 5", got)
	}
}
