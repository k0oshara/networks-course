package main

import (
    "context"
    "crypto/sha1"
    "encoding/hex"
    "encoding/json"
    "errors"
    "flag"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "net/url"
    "os"
    "os/signal"
    "path/filepath"
    "strings"
    "sync"
    "syscall"
    "time"
)

var hopByHopHeaders = map[string]struct{}{
    "Connection":          {},
    "Keep-Alive":          {},
    "Proxy-Authenticate":  {},
    "Proxy-Authorization": {},
    "Proxy-Connection":    {},
    "Te":                  {},
    "Trailer":             {},
    "Transfer-Encoding":   {},
    "Upgrade":             {},
}

type cacheEntry struct {
    URL          string              `json:"url"`
    StatusCode   int                 `json:"status_code"`
    Header       map[string][]string `json:"header"`
    ETag         string              `json:"etag,omitempty"`
    LastModified string              `json:"last_modified,omitempty"`
    BodyFile     string              `json:"body_file"`
    MetaFile     string              `json:"meta_file"`
    SavedAt      time.Time           `json:"saved_at"`
}

type diskCache struct {
    dir   string
    mu    sync.RWMutex
    items map[string]*cacheEntry
}

type proxyServer struct {
    client *http.Client
    logger *log.Logger
    cache  *diskCache
}

func main() {
    addr := flag.String("addr", ":8889", "proxy listen address")
    logPath := flag.String("log", "proxy_b.log", "path to proxy log file")
    cacheDir := flag.String("cache-dir", "cache_b", "directory for cached files")
    flag.Parse()

    logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
    if err != nil {
        log.Fatalf("open log file: %v", err)
    }
    defer logFile.Close()

    cache, err := newDiskCache(*cacheDir)
    if err != nil {
        log.Fatalf("create cache: %v", err)
    }

    proxy := &proxyServer{
        client: &http.Client{
            Timeout: 30 * time.Second,
            CheckRedirect: func(req *http.Request, via []*http.Request) error {
                return http.ErrUseLastResponse
            },
            Transport: &http.Transport{
                Proxy:                 nil,
                MaxIdleConns:          100,
                IdleConnTimeout:       90 * time.Second,
                TLSHandshakeTimeout:   10 * time.Second,
                ExpectContinueTimeout: 1 * time.Second,
            },
        },
        logger: log.New(logFile, "", log.LstdFlags),
        cache:  cache,
    }

    server := &http.Server{
        Addr:         *addr,
        Handler:      http.HandlerFunc(proxy.serveProxy),
        ReadTimeout:  15 * time.Second,
        WriteTimeout: 30 * time.Second,
        IdleTimeout:  60 * time.Second,
    }

    go func() {
        log.Printf("cache proxy listening on %s", *addr)
        if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
            log.Fatalf("proxy listen failed: %v", err)
        }
    }()

    stop := make(chan os.Signal, 1)
    signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
    <-stop

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    if err := server.Shutdown(ctx); err != nil {
        log.Printf("proxy shutdown failed: %v", err)
    }
}

func (p *proxyServer) serveProxy(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet && r.Method != http.MethodPost {
        p.respondWithError(w, r.Method, r.URL.String(), http.StatusMethodNotAllowed, "only GET and POST are supported")
        return
    }

    targetURL, err := parseTargetURL(r)
    if err != nil {
        p.respondWithError(w, r.Method, r.URL.String(), http.StatusBadRequest, err.Error())
        return
    }

    if r.Method == http.MethodPost {
        p.proxyWithoutCache(w, r, targetURL)
        return
    }

    cached := p.cache.get(targetURL.String())
    outReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, targetURL.String(), nil)
    if err != nil {
        p.respondWithError(w, r.Method, targetURL.String(), http.StatusBadRequest, fmt.Sprintf("create upstream request: %v", err))
        return
    }

    outReq.Header = copyClientHeaders(r.Header)
    outReq.Host = targetURL.Host
    addForwardedFor(outReq.Header, r.RemoteAddr)

    if cached != nil {
        if cached.ETag != "" {
            outReq.Header.Set("If-None-Match", cached.ETag)
        }
        if cached.LastModified != "" {
            outReq.Header.Set("If-Modified-Since", cached.LastModified)
        }
    }

    resp, err := p.client.Do(outReq)
    if err != nil {
        if cached != nil {
            p.serveFromCache(w, cached, "STALE", targetURL.String())
            return
        }
        p.respondWithError(w, r.Method, targetURL.String(), http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err))
        return
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotModified && cached != nil {
        p.serveFromCache(w, cached, "HIT", targetURL.String())
        return
    }

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        p.respondWithError(w, r.Method, targetURL.String(), http.StatusBadGateway, fmt.Sprintf("read upstream body: %v", err))
        return
    }

    copyServerHeaders(w.Header(), resp.Header)
    w.Header().Set("X-Proxy-Cache", "MISS")
    w.WriteHeader(resp.StatusCode)
    if _, err := w.Write(body); err != nil {
        p.logger.Printf("%s %s -> %d body_write_error=%q", r.Method, targetURL.String(), resp.StatusCode, err)
        return
    }

    if resp.StatusCode == http.StatusOK {
        entry := &cacheEntry{
            URL:          targetURL.String(),
            StatusCode:   resp.StatusCode,
            Header:       cloneHeaderMap(resp.Header),
            ETag:         resp.Header.Get("ETag"),
            LastModified: resp.Header.Get("Last-Modified"),
            SavedAt:      time.Now(),
        }
        if err := p.cache.save(entry, body); err != nil {
            p.logger.Printf("%s %s -> %d cache_save_error=%q", r.Method, targetURL.String(), resp.StatusCode, err)
        }
    }

    p.logger.Printf("%s %s -> %d cache=MISS", r.Method, targetURL.String(), resp.StatusCode)
}

func (p *proxyServer) proxyWithoutCache(w http.ResponseWriter, r *http.Request, targetURL *url.URL) {
    outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), r.Body)
    if err != nil {
        p.respondWithError(w, r.Method, targetURL.String(), http.StatusBadRequest, fmt.Sprintf("create upstream request: %v", err))
        return
    }

    outReq.Header = copyClientHeaders(r.Header)
    outReq.Host = targetURL.Host
    outReq.ContentLength = r.ContentLength
    outReq.Close = false
    addForwardedFor(outReq.Header, r.RemoteAddr)

    resp, err := p.client.Do(outReq)
    if err != nil {
        p.respondWithError(w, r.Method, targetURL.String(), http.StatusBadGateway, fmt.Sprintf("upstream request failed: %v", err))
        return
    }
    defer resp.Body.Close()

    copyServerHeaders(w.Header(), resp.Header)
    w.Header().Set("X-Proxy-Cache", "BYPASS")
    w.WriteHeader(resp.StatusCode)
    if _, err := io.Copy(w, resp.Body); err != nil {
        p.logger.Printf("%s %s -> %d body_copy_error=%q", r.Method, targetURL.String(), resp.StatusCode, err)
        return
    }

    p.logger.Printf("%s %s -> %d cache=BYPASS", r.Method, targetURL.String(), resp.StatusCode)
}

func parseTargetURL(r *http.Request) (*url.URL, error) {
    if r.URL == nil {
        return nil, errors.New("missing request URL")
    }

    if r.URL.IsAbs() {
        if r.URL.Scheme != "http" {
            return nil, fmt.Errorf("unsupported scheme %q", r.URL.Scheme)
        }
        return r.URL, nil
    }

    rawTarget := strings.TrimPrefix(r.URL.Path, "/")
    if rawTarget == "" {
        return nil, errors.New("target URL is empty")
    }

    if r.URL.RawQuery != "" {
        rawTarget += "?" + r.URL.RawQuery
    }

    if !strings.HasPrefix(rawTarget, "http://") {
        rawTarget = "http://" + rawTarget
    }

    targetURL, err := url.Parse(rawTarget)
    if err != nil {
        return nil, fmt.Errorf("parse target URL: %w", err)
    }

    if targetURL.Host == "" {
        return nil, errors.New("target host is empty")
    }

    if targetURL.Scheme != "http" {
        return nil, fmt.Errorf("unsupported scheme %q", targetURL.Scheme)
    }

    return targetURL, nil
}

func copyClientHeaders(src http.Header) http.Header {
    dst := make(http.Header, len(src))
    blocked := connectionTokens(src)
    for key, values := range src {
        if isProxyHopHeader(key) || blocked[http.CanonicalHeaderKey(key)] {
            continue
        }
        for _, value := range values {
            dst.Add(key, value)
        }
    }
    return dst
}

func copyServerHeaders(dst, src http.Header) {
    blocked := connectionTokens(src)
    for key, values := range src {
        if isProxyHopHeader(key) || blocked[http.CanonicalHeaderKey(key)] {
            continue
        }
        for _, value := range values {
            dst.Add(key, value)
        }
    }
}

func isProxyHopHeader(name string) bool {
    _, ok := hopByHopHeaders[http.CanonicalHeaderKey(name)]
    return ok
}

func connectionTokens(headers http.Header) map[string]bool {
    result := make(map[string]bool)
    for _, value := range headers.Values("Connection") {
        for _, token := range strings.Split(value, ",") {
            token = strings.TrimSpace(token)
            if token == "" {
                continue
            }
            result[http.CanonicalHeaderKey(token)] = true
        }
    }
    return result
}

func addForwardedFor(headers http.Header, remoteAddr string) {
    host, _, err := net.SplitHostPort(remoteAddr)
    if err != nil {
        host = remoteAddr
    }

    if previous := headers.Get("X-Forwarded-For"); previous != "" {
        headers.Set("X-Forwarded-For", previous+", "+host)
        return
    }
    headers.Set("X-Forwarded-For", host)
}

func cloneHeaderMap(src http.Header) map[string][]string {
    dst := make(map[string][]string, len(src))
    blocked := connectionTokens(src)
    for key, values := range src {
        if isProxyHopHeader(key) || blocked[http.CanonicalHeaderKey(key)] {
            continue
        }
        copied := make([]string, len(values))
        copy(copied, values)
        dst[key] = copied
    }
    return dst
}

func newDiskCache(dir string) (*diskCache, error) {
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return nil, err
    }

    cache := &diskCache{
        dir:   dir,
        items: make(map[string]*cacheEntry),
    }

    if err := cache.loadSavedEntries(); err != nil {
        return nil, err
    }

    return cache, nil
}

func (c *diskCache) loadSavedEntries() error {
    metaFiles, err := filepath.Glob(filepath.Join(c.dir, "*.meta.json"))
    if err != nil {
        return err
    }

    for _, metaPath := range metaFiles {
        data, err := os.ReadFile(metaPath)
        if err != nil {
            continue
        }

        var entry cacheEntry
        if err := json.Unmarshal(data, &entry); err != nil {
            continue
        }

        if _, err := os.Stat(entry.BodyFile); err != nil {
            continue
        }

        c.items[entry.URL] = &entry
    }

    return nil
}

func (c *diskCache) get(rawURL string) *cacheEntry {
    c.mu.RLock()
    defer c.mu.RUnlock()

    entry, ok := c.items[rawURL]
    if !ok {
        return nil
    }

    copied := *entry
    copied.Header = cloneHeaderMap(http.Header(entry.Header))
    return &copied
}

func (c *diskCache) save(entry *cacheEntry, body []byte) error {
    key := hashURL(entry.URL)
    entry.BodyFile = filepath.Join(c.dir, key+".body")
    entry.MetaFile = filepath.Join(c.dir, key+".meta.json")

    if err := os.WriteFile(entry.BodyFile, body, 0o644); err != nil {
        return err
    }

    meta, err := json.MarshalIndent(entry, "", "  ")
    if err != nil {
        return err
    }

    if err := os.WriteFile(entry.MetaFile, meta, 0o644); err != nil {
        return err
    }

    c.mu.Lock()
    defer c.mu.Unlock()
    copied := *entry
    copied.Header = cloneHeaderMap(http.Header(entry.Header))
    c.items[entry.URL] = &copied
    return nil
}

func hashURL(rawURL string) string {
    sum := sha1.Sum([]byte(rawURL))
    return hex.EncodeToString(sum[:])
}

func (p *proxyServer) serveFromCache(w http.ResponseWriter, entry *cacheEntry, cacheState, rawURL string) {
    body, err := os.ReadFile(entry.BodyFile)
    if err != nil {
        p.respondWithError(w, http.MethodGet, rawURL, http.StatusBadGateway, fmt.Sprintf("read cache body: %v", err))
        return
    }

    copyServerHeaders(w.Header(), http.Header(entry.Header))
    w.Header().Set("X-Proxy-Cache", cacheState)
    w.WriteHeader(entry.StatusCode)
    if _, err := w.Write(body); err != nil {
        p.logger.Printf("GET %s -> %d cache=%s body_write_error=%q", rawURL, entry.StatusCode, cacheState, err)
        return
    }

    p.logger.Printf("GET %s -> %d cache=%s", rawURL, entry.StatusCode, cacheState)
}

func (p *proxyServer) respondWithError(w http.ResponseWriter, method, rawURL string, status int, message string) {
    http.Error(w, message, status)
    p.logger.Printf("%s %s -> %d error=%q", method, rawURL, status, message)
}
