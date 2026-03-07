package product

import "errors"

var (
    ErrNotFound        = errors.New("product not found")
    ErrInvalidInput    = errors.New("invalid input")
    ErrNothingToUpdate = errors.New("nothing to update")
)

type Product struct {
    ID          int    `json:"id"`
    Name        string `json:"name"`
    Description string `json:"description"`
    Icon        string `json:"icon"`
}

type CreateRequest struct {
    Name        string `json:"name"`
    Description string `json:"description"`
}

type UpdateRequest struct {
    ID          *int    `json:"id,omitempty"`
    Name        *string `json:"name,omitempty"`
    Description *string `json:"description,omitempty"`
    Icon        *string `json:"icon,omitempty"`
}

type ErrorResponse struct {
    Error string `json:"error"`
}

type Store interface {
    Create(name, description string) (Product, error)
    Get(id int) (Product, error)
    List() ([]Product, error)
    Update(id int, req UpdateRequest) (Product, error)
    SetIcon(id int, icon string) (Product, string, error)
    Delete(id int) (Product, error)
}
