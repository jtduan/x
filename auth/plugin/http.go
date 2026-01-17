package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/core/auth"
	"github.com/go-gost/core/logger"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/plugin"
)

type httpAuthCacheEntry struct {
	id      string
	ok      bool
	expires time.Time
}

type httpPluginRequest struct {
	Service  string `json:"service"`
	Username string `json:"username"`
	Password string `json:"password"`
	Client   string `json:"client"`
}

type httpPluginResponse struct {
	OK bool   `json:"ok"`
	ID string `json:"id"`
}

type httpPlugin struct {
	url    string
	client *http.Client
	header http.Header
	log    logger.Logger
	mu     sync.Mutex
	cache  map[string]httpAuthCacheEntry
}

// NewHTTPPlugin creates an Authenticator plugin based on HTTP.
func NewHTTPPlugin(name string, url string, opts ...plugin.Option) auth.Authenticator {
	var options plugin.Options
	for _, opt := range opts {
		opt(&options)
	}

	return &httpPlugin{
		url:    url,
		client: plugin.NewHTTPClient(&options),
		header: options.Header,
		log: logger.Default().WithFields(map[string]any{
			"kind":   "auther",
			"auther": name,
		}),
		cache: make(map[string]httpAuthCacheEntry),
	}
}

func (p *httpPlugin) Authenticate(ctx context.Context, user, password string, opts ...auth.Option) (id string, ok bool) {
	if p.client == nil {
		return
	}

	cacheKey := user
	if s, _, found := strings.Cut(user, "-"); found {
		cacheKey = s
	}
	username := user
	if cacheKey != "" {
		p.mu.Lock()
		if ent, ok := p.cache[cacheKey]; ok {
			if time.Now().Before(ent.expires) {
				p.mu.Unlock()
				return username, ent.ok
			}
			delete(p.cache, cacheKey)
		}
		p.mu.Unlock()
	}

	var options auth.Options
	for _, opt := range opts {
		opt(&options)
	}

	var clientAddr string
	if v := xctx.SrcAddrFromContext(ctx); v != nil {
		clientAddr = v.String()
	}

	rb := httpPluginRequest{
		Service:  options.Service,
		Username: username,
		Password: password,
		Client:   clientAddr,
	}
	v, err := json.Marshal(&rb)
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(v))
	if err != nil {
		return
	}

	if p.header != nil {
		req.Header = p.header.Clone()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	res := httpPluginResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return
	}
	if cacheKey != "" && res.OK {
		p.mu.Lock()
		p.cache[cacheKey] = httpAuthCacheEntry{
			id:      res.ID,
			ok:      res.OK,
			expires: time.Now().Add(5 * time.Minute),
		}
		p.mu.Unlock()
	}
	return res.ID, res.OK
}
