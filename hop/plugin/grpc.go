package hop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/hop"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/plugin/hop/proto"
	"github.com/go-gost/x/config"
	node_parser "github.com/go-gost/x/config/parsing/node"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/plugin"
	"google.golang.org/grpc"
)

type grpcPlugin struct {
	name   string
	addr   string
	conn   grpc.ClientConnInterface
	client proto.HopClient
	log    logger.Logger
}

type statusError struct {
	statusCode int
	err        error
}

func (e *statusError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return http.StatusText(e.statusCode)
}

func (e *statusError) StatusCode() int {
	return e.statusCode
}

// NewGRPCPlugin creates a Hop plugin based on gRPC.
func NewGRPCPlugin(name string, addr string, opts ...plugin.Option) hop.Hop {
	var options plugin.Options
	for _, opt := range opts {
		opt(&options)
	}

	log := logger.Default().WithFields(map[string]any{
		"kind": "hop",
		"hop":  name,
	})
	conn, err := plugin.NewGRPCConn(addr, &options)
	if err != nil {
		log.Error(err)
	}

	p := &grpcPlugin{
		name: name,
		addr: addr,
		conn: conn,
		log:  log,
	}
	if conn != nil {
		p.client = proto.NewHopClient(conn)
	}
	return p
}

func (p *grpcPlugin) Select(ctx context.Context, opts ...hop.SelectOption) *chain.Node {
	if p.client == nil {
		return nil
	}

	var options hop.SelectOptions
	for _, opt := range opts {
		opt(&options)
	}

	var srcAddr string
	if addr := xctx.SrcAddrFromContext(ctx); addr != nil {
		srcAddr = addr.String()
	}
	r, err := p.client.Select(ctx,
		&proto.SelectRequest{
			Network: options.Network,
			Addr:    options.Addr,
			Host:    options.Host,
			Path:    options.Path,
			Client:  xctx.ClientIDFromContext(ctx).String(),
			Src:     srcAddr,
		})
	if err != nil {
		p.log.Error(err)
		return chain.NewNode(p.name, p.addr,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: %w", p.name, err),
				},
			}),
		)
	}
	if r == nil || len(r.Node) == 0 {
		return chain.NewNode(p.name, p.addr,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: empty response", p.name),
				},
			}),
		)
	}

	var cfg config.NodeConfig
	if err := json.NewDecoder(bytes.NewReader(r.Node)).Decode(&cfg); err != nil {
		p.log.Error(err)
		return chain.NewNode(p.name, p.addr,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: %w", p.name, err),
				},
			}),
		)
	}

	node, err := node_parser.ParseNode(p.name, &cfg, logger.Default())
	if err != nil {
		p.log.Error(err)
		return chain.NewNode(p.name, p.addr,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: %w", p.name, err),
				},
			}),
		)
	}
	return node
}

func (p *grpcPlugin) Close() error {
	if p.conn == nil {
		return nil
	}

	if closer, ok := p.conn.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
