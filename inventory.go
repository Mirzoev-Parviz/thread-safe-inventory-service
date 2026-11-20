// Package inventory provides a thread-safe in-memory inventory.
package inventory

import (
	"errors"
	"fmt"
	"sync"
)

// Errors returned by SafeInventoryService.
var (
	ErrProductNotFound   = errors.New("product not found")
	ErrInsufficientStock = errors.New("insufficient stock")
	ErrInvalidQuantity   = errors.New("quantity must be positive")
)

// Product is a single inventory item.
type Product struct {
	ID    string
	Name  string
	Stock int
}

// ReserveItem is one line of a multi-product reservation.
type ReserveItem struct {
	ProductID string
	Quantity  int
}

// SafeInventoryService is an inventory safe for concurrent use.
type SafeInventoryService struct {
	mu       sync.RWMutex
	products map[string]*Product
}

// NewSafeInventoryService creates a service with copies of the given products.
func NewSafeInventoryService(products ...Product) *SafeInventoryService {
	s := &SafeInventoryService{products: make(map[string]*Product, len(products))}
	for _, p := range products {
		s.products[p.ID] = &p
	}
	return s
}

// GetStock returns the product's stock, or 0 if the product is unknown.
func (s *SafeInventoryService) GetStock(productID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	product, ok := s.products[productID]
	if !ok {
		return 0
	}
	return product.Stock
}

// Reserve atomically checks and decrements stock.
func (s *SafeInventoryService) Reserve(productID string, quantity int) error {
	if quantity <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidQuantity, quantity)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	product, ok := s.products[productID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrProductNotFound, productID)
	}
	if product.Stock < quantity {
		return fmt.Errorf("%w: product %q has %d, requested %d",
			ErrInsufficientStock, productID, product.Stock, quantity)
	}

	product.Stock -= quantity
	return nil
}

// ReserveMultiple reserves all items or none of them.
// Quantities of repeated product IDs are summed.
func (s *SafeInventoryService) ReserveMultiple(items []ReserveItem) error {
	totals := make(map[string]int, len(items))
	for _, item := range items {
		if item.Quantity <= 0 {
			return fmt.Errorf("%w: product %q, got %d", ErrInvalidQuantity, item.ProductID, item.Quantity)
		}
		totals[item.ProductID] += item.Quantity
	}
	if len(totals) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for id, qty := range totals {
		product, ok := s.products[id]
		if !ok {
			return fmt.Errorf("%w: %q", ErrProductNotFound, id)
		}
		if product.Stock < qty {
			return fmt.Errorf("%w: product %q has %d, requested %d",
				ErrInsufficientStock, id, product.Stock, qty)
		}
	}

	for id, qty := range totals {
		s.products[id].Stock -= qty
	}
	return nil
}
