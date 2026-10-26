package main

import (
	"encoding/json/v2"
	"io"
	"net/url"

	"github.com/spf13/cobra"
	"go.kenn.io/benchdb/sdk/go/benchdb"
)

func ciReevaluateCommand(stdout io.Writer) *cobra.Command {
	var server, token string
	cmd := configureCommand(&cobra.Command{
		Use:   "reevaluate RUN_ID",
		Short: "Save a new evaluation using a run's original baseline and measurements.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if server == "" {
				return commandUsageError(cmd, "--server is required")
			}
			bearer, err := resolveBearer(token, server)
			if err != nil {
				return err
			}
			client, err := newClient(server)
			if err != nil {
				return err
			}
			response, err := client.ReevaluateRunReportWithResponse(cmd.Context(), &benchdb.ReevaluateRunReportRequestOptions{PathParams: &benchdb.ReevaluateRunReportPath{RunID: url.PathEscape(args[0])}}, bearerRequestEditor(bearer))
			if err != nil && (response == nil || response.StatusCode/100 == 2) {
				return err
			}
			if response.JSON200 == nil {
				return statusError(response.HTTPResponse, response.Body)
			}
			return json.MarshalWrite(stdout, response.JSON200)
		},
	})
	cmd.Flags().StringVar(&server, "server", "", "BenchDB server URL")
	cmd.Flags().StringVar(&token, "token", "", "API token (or saved login)")
	return cmd
}
