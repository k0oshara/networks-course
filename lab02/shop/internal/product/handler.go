package product

import (
    "errors"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "strconv"
    "strings"

    "encoding/json"
    "fmt"
    "log"
)

type Handler struct {
    st Store
}

func NewHandler(st Store) *Handler {
    return &Handler{st: st}
}

func (h *Handler) Register(mux *http.ServeMux) {
    mux.HandleFunc("/products", h.handleProducts)
    mux.HandleFunc("/product", h.handleProduct)
    mux.HandleFunc("/product/", h.handleProductByID)
}


func DecodeJSON(r *http.Request, dst any) error {
    defer r.Body.Close()

    dec := json.NewDecoder(r.Body)
    dec.DisallowUnknownFields()

    if err := dec.Decode(dst); err != nil {
        return fmt.Errorf("invalid json: %w", err)
    }

    if dec.More() {
        return fmt.Errorf("invalid json: multiple json values in body")
    }

    return nil
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, msg string) {
    WriteJSON(w, status, ErrorResponse{Error: msg})
}

func LoggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        log.Printf("%s %s", r.Method, r.URL.Path)
        next.ServeHTTP(w, r)
    })
}


func (h *Handler) handleProducts(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
        return
    }

    ps, err := h.st.List()
    if err != nil {
        WriteError(w, http.StatusInternalServerError, "internal server error")
        return
    }

    WriteJSON(w, http.StatusOK, ps)
}

func (h *Handler) handleProduct(w http.ResponseWriter, r *http.Request) {
    if r.URL.Path != "/product" {
        http.NotFound(w, r)
        return
    }

    if r.Method != http.MethodPost {
        WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
        return
    }

    var req CreateRequest
    if err := DecodeJSON(r, &req); err != nil {
        WriteError(w, http.StatusBadRequest, err.Error())
        return
    }

    if strings.TrimSpace(req.Name) == "" {
        h.writeProductError(w, ErrInvalidInput)
        return
    }

    p, err := h.st.Create(req.Name, req.Description)
    if err != nil {
        h.writeProductError(w, err)
        return
    }

    WriteJSON(w, http.StatusCreated, p)
}

func (h *Handler) handleProductByID(w http.ResponseWriter, r *http.Request) {
    if strings.HasSuffix(r.URL.Path, "/image") {
        h.handleProductImage(w, r)
        return
    }

    id, err := productIDFromPath(r.URL.Path, "")
    if err != nil {
        WriteError(w, http.StatusBadRequest, "invalid product id")
        return
    }

    switch r.Method {
    case http.MethodGet:
        if id <= 0 {
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        p, err := h.st.Get(id)
        if err != nil {
            h.writeProductError(w, err)
            return
        }
        WriteJSON(w, http.StatusOK, p)

    case http.MethodPut:
        var req UpdateRequest
        if err := DecodeJSON(r, &req); err != nil {
            WriteError(w, http.StatusBadRequest, err.Error())
            return
        }

        if id <= 0 {
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        if req.ID != nil && *req.ID != id {
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        if req.Icon != nil {
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        if req.Name == nil && req.Description == nil {
            h.writeProductError(w, ErrNothingToUpdate)
            return
        }

        p, err := h.st.Update(id, req)
        if err != nil {
            h.writeProductError(w, err)
            return
        }

        WriteJSON(w, http.StatusOK, p)

    case http.MethodDelete:
        if id <= 0 {
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        p, err := h.st.Delete(id)
        if err != nil {
            h.writeProductError(w, err)
            return
        }

        WriteJSON(w, http.StatusOK, p)

    default:
        WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
    }
}

func (h *Handler) handleProductImage(w http.ResponseWriter, r *http.Request) {
    id, err := productIDFromPath(r.URL.Path, "/image")
    if err != nil {
        WriteError(w, http.StatusBadRequest, "invalid product id")
        return
    }

    switch r.Method {
    case http.MethodPost:
        f, hdr, err := r.FormFile("icon")
        if err != nil {
            WriteError(w, http.StatusBadRequest, "icon file is required")
            return
        }
        defer f.Close()

        if id <= 0 {
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        if _, err := h.st.Get(id); err != nil {
            h.writeProductError(w, err)
            return
        }

        if err := os.MkdirAll("uploads", 0o755); err != nil {
            WriteError(w, http.StatusInternalServerError, "internal server error")
            return
        }

        ext := strings.ToLower(filepath.Ext(hdr.Filename))
        if ext == "" {
            ext = ".bin"
        }

        path := filepath.Join("uploads", strconv.Itoa(id)+ext)

        out, err := os.Create(path)
        if err != nil {
            WriteError(w, http.StatusInternalServerError, "internal server error")
            return
        }

        if _, err := io.Copy(out, f); err != nil {
            out.Close()
            _ = os.Remove(path)
            WriteError(w, http.StatusInternalServerError, "internal server error")
            return
        }

        if err := out.Close(); err != nil {
            _ = os.Remove(path)
            WriteError(w, http.StatusInternalServerError, "internal server error")
            return
        }

        if strings.TrimSpace(path) == "" {
            _ = os.Remove(path)
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        p, old, err := h.st.SetIcon(id, path)
        if err != nil {
            _ = os.Remove(path)
            h.writeProductError(w, err)
            return
        }

        if old != "" && old != path {
            _ = os.Remove(old)
        }

        WriteJSON(w, http.StatusOK, p)

    case http.MethodGet:
        if id <= 0 {
            h.writeProductError(w, ErrInvalidInput)
            return
        }

        p, err := h.st.Get(id)
        if err != nil {
            h.writeProductError(w, err)
            return
        }

        if strings.TrimSpace(p.Icon) == "" {
            WriteError(w, http.StatusNotFound, "product image not found")
            return
        }

        http.ServeFile(w, r, p.Icon)

    default:
        WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
    }
}

func (h *Handler) writeProductError(w http.ResponseWriter, err error) {
    switch {
    case errors.Is(err, ErrNotFound):
        WriteError(w, http.StatusNotFound, err.Error())
    case errors.Is(err, ErrInvalidInput), errors.Is(err, ErrNothingToUpdate):
        WriteError(w, http.StatusBadRequest, err.Error())
    default:
        WriteError(w, http.StatusInternalServerError, "internal server error")
    }
}

func productIDFromPath(path, suffix string) (int, error) {
    s := strings.TrimPrefix(path, "/product/")

    if suffix != "" {
        if !strings.HasSuffix(s, suffix) {
            return 0, strconv.ErrSyntax
        }
        s = strings.TrimSuffix(s, suffix)
    }

    if s == "" || strings.Contains(s, "/") {
        return 0, strconv.ErrSyntax
    }

    return strconv.Atoi(s)
}
