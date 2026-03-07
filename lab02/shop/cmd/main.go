package main

import (
    "log"
    "net/http"
    "os"

    "shop/internal/product"
    "shop/internal/storage"
)

func main() {
    if err := os.MkdirAll("uploads", 0o755); err != nil {
        log.Fatal(err)
    }

    st := storage.NewProductStore()
    h := product.NewHandler(st)

    mux := http.NewServeMux()
    h.Register(mux)

    log.Println("server started at :8080")
    log.Fatal(http.ListenAndServe(":8080", product.LoggingMiddleware(mux)))
}
