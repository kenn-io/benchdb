package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/benchdb/sdk/go/benchdb"
)

// healthCommand probes process liveness without loading database or auth config.
func healthCommand(stdout io.Writer) *cobra.Command {
	serverURL := "http://127.0.0.1:8080"
	timeout := 5 * time.Second
	cmd := configureCommand(&cobra.Command{
		Use:   "health",
		Short: "Check server liveness without authentication.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return commandUsageError(cmd, "unexpected argument %q", args[0])
			}
			if timeout <= 0 {
				return commandUsageError(cmd, "--timeout must be positive")
			}
			u, err := url.Parse(serverURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
				return commandUsageError(cmd, "--server must be an absolute HTTP(S) URL without credentials, query, or fragment")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			httpClient := &http.Client{
				Timeout:       timeout,
				Transport:     healthTransport{http.DefaultTransport},
				CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
			}
			client, err := benchdb.NewHTTPClient(strings.TrimRight(serverURL, "/"), httpClient)
			if err != nil {
				return errors.New("cannot create health client")
			}
			resp, err := client.PingWithResponse(cmd.Context())
			if resp != nil && resp.StatusCode != http.StatusOK {
				return fmt.Errorf("health returned HTTP %d", resp.StatusCode)
			}
			if err != nil || resp == nil || len(resp.Body) > 4096 || resp.JSON200 == nil || resp.JSON200.Status != "ok" {
				return errors.New("health response failed, timed out, or is not ok within 4 KiB")
			}
			_, err = fmt.Fprintln(stdout, "ok")
			return err
		},
	})
	cmd.Flags().StringVar(&serverURL, "server", serverURL, "server base URL, including any application path prefix")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "overall health request timeout")
	return cmd
}

// Bound reads before the generated client buffers and decodes the response.
type healthTransport struct{ base http.RoundTripper }

func (t healthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(resp.Body, 4097), resp.Body}
	}
	return resp, err
}
