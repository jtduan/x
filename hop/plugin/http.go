package hop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/go-gost/core/chain"
	"github.com/go-gost/core/connector"
	"github.com/go-gost/core/hop"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/config"
	node_parser "github.com/go-gost/x/config/parsing/node"
	xctx "github.com/go-gost/x/ctx"
	"github.com/go-gost/x/internal/plugin"
)

type httpPluginRequest struct {
	Network string `json:"network"`
	Addr    string `json:"addr"`
	Protocol string `json:"protocol"`
	Host    string `json:"host"`
	Path    string `json:"path"`
	Client  string `json:"client"`
	Src     string `json:"src"`
}

type httpPlugin struct {
	name   string
	url    string
	client *http.Client
	header http.Header
	log    logger.Logger
}

type failingTransport struct {
	err error
}

func (tr *failingTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	return nil, tr.err
}

func (tr *failingTransport) Handshake(ctx context.Context, conn net.Conn) (net.Conn, error) {
	return nil, tr.err
}

func (tr *failingTransport) Connect(ctx context.Context, conn net.Conn, network, address string) (net.Conn, error) {
	return nil, tr.err
}

func (tr *failingTransport) Bind(ctx context.Context, conn net.Conn, network, address string, opts ...connector.BindOption) (net.Listener, error) {
	return nil, connector.ErrBindUnsupported
}

func (tr *failingTransport) Multiplex() bool {
	return false
}

func (tr *failingTransport) Options() *chain.TransportOptions {
	return &chain.TransportOptions{}
}

func (tr *failingTransport) Copy() chain.Transporter {
	tr2 := &failingTransport{}
	*tr2 = *tr
	return tr2
}

// NewHTTPPlugin creates an Hop plugin based on HTTP.
func NewHTTPPlugin(name string, url string, opts ...plugin.Option) hop.Hop {
	var options plugin.Options
	for _, opt := range opts {
		opt(&options)
	}

	return &httpPlugin{
		name:   name,
		url:    url,
		client: plugin.NewHTTPClient(&options),
		header: options.Header,
		log: logger.Default().WithFields(map[string]any{
			"kind": "hop",
			"hop":  name,
		}),
	}
}

func (p *httpPlugin) Select(ctx context.Context, opts ...hop.SelectOption) *chain.Node {
	if p.client == nil {
		return chain.NewNode(p.name, p.url,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: http client is nil", p.name),
				},
			}),
		)
	}

	var options hop.SelectOptions
	for _, opt := range opts {
		opt(&options)
	}

	var srcAddr string
	if addr := xctx.SrcAddrFromContext(ctx); addr != nil {
		srcAddr = addr.String()
	}

	rb := httpPluginRequest{
		Network: options.Network,
		Addr:    options.Addr,
		Protocol: options.Protocol,
		Host:    options.Host,
		Path:    options.Path,
		Client:  xctx.ClientIDFromContext(ctx).String(),
		Src:     srcAddr,
	}
	v, err := json.Marshal(&rb)
	if err != nil {
		p.log.Error(err)
		return chain.NewNode(p.name, p.url,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: %w", p.name, err),
				},
			}),
		)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(v))
	if err != nil {
		p.log.Error(err)
		return chain.NewNode(p.name, p.url,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: %w", p.name, err),
				},
			}),
		)
	}

	if p.header != nil {
		req.Header = p.header.Clone()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Error(err)
		return chain.NewNode(p.name, p.url,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: %w", p.name, err),
				},
			}),
		)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		p.log.Error(resp.Status)
		return chain.NewNode(p.name, p.url,
			chain.TransportNodeOption(&failingTransport{
				err: &statusError{
					statusCode: 451,
					err:        fmt.Errorf("hop plugin %s: %s", p.name, resp.Status),
				},
			}),
		)
	}

	var cfg config.NodeConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		p.log.Error(err)
		return chain.NewNode(p.name, p.url,
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
		return chain.NewNode(p.name, p.url,
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
