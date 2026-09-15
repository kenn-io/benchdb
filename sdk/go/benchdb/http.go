package benchdb

import (
	"context"
	"net/http"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
)

// NewHTTPClient connects the generated operations to a standard HTTP client.
func NewHTTPClient(server string, client *http.Client) (*Client, error) {
	api, err := runtime.NewAPIClient(server, runtime.WithHTTPClient(httpDoer{client}))
	if err != nil {
		return nil, err
	}
	return NewClient(api), nil
}

type httpDoer struct{ client *http.Client }

func (d httpDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	return d.client.Do(req.WithContext(ctx))
}
