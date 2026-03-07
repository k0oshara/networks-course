package storage

import (
    "sort"
    "sync"

    "shop/internal/product"
)

type ProductStore struct {
    mu       sync.RWMutex
    nextID   int
    products map[int]product.Product
}

func NewProductStore() *ProductStore {
    return &ProductStore{
        nextID:   1,
        products: make(map[int]product.Product),
    }
}

var _ product.Store = (*ProductStore)(nil)

func (s *ProductStore) Create(name, description string) (product.Product, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    p := product.Product{
        ID:          s.nextID,
        Name:        name,
        Description: description,
        Icon:        "",
    }

    s.products[p.ID] = p
    s.nextID++

    return p, nil
}

func (s *ProductStore) Get(id int) (product.Product, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    p, ok := s.products[id]
    if !ok {
        return product.Product{}, product.ErrNotFound
    }

    return p, nil
}

func (s *ProductStore) List() ([]product.Product, error) {
    s.mu.RLock()
    defer s.mu.RUnlock()

    out := make([]product.Product, 0, len(s.products))
    for _, p := range s.products {
        out = append(out, p)
    }

    sort.Slice(out, func(i, j int) bool {
        return out[i].ID < out[j].ID
    })

    return out, nil
}

func (s *ProductStore) Update(id int, req product.UpdateRequest) (product.Product, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    p, ok := s.products[id]
    if !ok {
        return product.Product{}, product.ErrNotFound
    }

    if req.Name != nil {
        p.Name = *req.Name
    }
    if req.Description != nil {
        p.Description = *req.Description
    }

    s.products[id] = p
    return p, nil
}

func (s *ProductStore) SetIcon(id int, icon string) (product.Product, string, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    p, ok := s.products[id]
    if !ok {
        return product.Product{}, "", product.ErrNotFound
    }

    old := p.Icon
    p.Icon = icon
    s.products[id] = p

    return p, old, nil
}

func (s *ProductStore) Delete(id int) (product.Product, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    p, ok := s.products[id]
    if !ok {
        return product.Product{}, product.ErrNotFound
    }

    delete(s.products, id)
    return p, nil
}
