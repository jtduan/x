package auth

import (
	"context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/core/auth"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/plugin/auth/proto"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/plugin"
	"google.golang.org/grpc"
)

type grpcAuthCacheEntry struct {
	id      string
	user    string
	pass    string
	ok      bool
	expires time.Time
}

type grpcPlugin struct {
	conn   grpc.ClientConnInterface
	client proto.AuthenticatorClient
	log    logger.Logger
	mu     sync.Mutex
	cache  map[string]grpcAuthCacheEntry
}

// NewGRPCPlugin creates an Authenticator plugin based on gRPC.
func NewGRPCPlugin(name string, addr string, opts ...plugin.Option) auth.Authenticator {
	var options plugin.Options
	for _, opt := range opts {
		opt(&options)
	}

	log := logger.Default().WithFields(map[string]any{
		"kind":   "auther",
		"auther": name,
	})
	conn, err := plugin.NewGRPCConn(addr, &options)
	if err != nil {
		log.Error(err)
	}

	p := &grpcPlugin{
		conn: conn,
		log:  log,
		cache: make(map[string]grpcAuthCacheEntry),
	}

	if conn != nil {
		p.client = proto.NewAuthenticatorClient(conn)
	}
	return p
}

// Authenticate checks the validity of the provided user-password pair.
func (p *grpcPlugin) Authenticate(ctx context.Context, user, password string, opts ...auth.Option) (string, bool) {
	if p.client == nil {
		return "", false
	}
	if user == "" {
		return "", false
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
				if ent.pass == password {
					p.mu.Unlock()
					return username, ent.ok
				}
				p.log.Infof("authenticate: cache mismatch key=%q req_user=%q ent_user=%q req_pass_len=%d ent_pass_len=%d", cacheKey, username, ent.user, len(password), len(ent.pass))
				p.mu.Unlock()
				return username, false
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
	r, err := p.client.Authenticate(ctx,
		&proto.AuthenticateRequest{
			Service:  options.Service,
			Username: username,
			Password: password,
			Client:   clientAddr,
		})
	if err != nil {
		p.log.Error(err)
		return "", false
	}
	if cacheKey != "" && r.Ok {
		p.mu.Lock()
		p.cache[cacheKey] = grpcAuthCacheEntry{
			id:      r.Id,
			user:    username,
			pass:    password,
			ok:      r.Ok,
			expires: time.Now().Add(5 * time.Minute),
		}
		p.mu.Unlock()
	}
	return username, r.Ok
}

func (p *grpcPlugin) Close() error {
	if closer, ok := p.conn.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
