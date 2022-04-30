package cli

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/spf13/cobra"
	"golang.org/x/xerrors"

	"cdr.dev/slog"
	"cdr.dev/slog/sloggers/sloghuman"

	"github.com/coder/coder/agent"
	"github.com/coder/coder/cli/cliflag"
	"github.com/coder/coder/codersdk"
	"github.com/coder/retry"
)

func workspaceAgent() *cobra.Command {
	var (
		auth string
	)
	cmd := &cobra.Command{
		Use: "agent",
		// This command isn't useful to manually execute.
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rawURL, err := cmd.Flags().GetString(varAgentURL)
			if err != nil {
				return xerrors.Errorf("CODER_AGENT_URL must be set: %w", err)
			}
			coderURL, err := url.Parse(rawURL)
			if err != nil {
				return xerrors.Errorf("parse %q: %w", rawURL, err)
			}
			logger := slog.Make(sloghuman.Sink(cmd.OutOrStdout())).Leveled(slog.LevelDebug)
			client := codersdk.New(coderURL)

			// exchangeToken returns a session token.
			// This is abstracted to allow for the same looping condition
			// regardless of instance identity auth type.
			var exchangeToken func(context.Context) (codersdk.WorkspaceAgentAuthenticateResponse, error)
			switch auth {
			case "token":
				token, err := cmd.Flags().GetString(varAgentToken)
				if err != nil {
					return xerrors.Errorf("CODER_AGENT_TOKEN must be set for token auth: %w", err)
				}
				client.SessionToken = token
			case "google-instance-identity":
				// This is *only* done for testing to mock client authentication.
				// This will never be set in a production scenario.
				var gcpClient *metadata.Client
				gcpClientRaw := cmd.Context().Value("gcp-client")
				if gcpClientRaw != nil {
					gcpClient, _ = gcpClientRaw.(*metadata.Client)
				}
				exchangeToken = func(ctx context.Context) (codersdk.WorkspaceAgentAuthenticateResponse, error) {
					return client.AuthWorkspaceGoogleInstanceIdentity(ctx, "", gcpClient)
				}
			case "aws-instance-identity":
				// This is *only* done for testing to mock client authentication.
				// This will never be set in a production scenario.
				var awsClient *http.Client
				awsClientRaw := cmd.Context().Value("aws-client")
				if awsClientRaw != nil {
					awsClient, _ = awsClientRaw.(*http.Client)
					if awsClient != nil {
						client.HTTPClient = awsClient
					}
				}
				exchangeToken = func(ctx context.Context) (codersdk.WorkspaceAgentAuthenticateResponse, error) {
					return client.AuthWorkspaceAWSInstanceIdentity(ctx)
				}
			case "azure-instance-identity":
				// This is *only* done for testing to mock client authentication.
				// This will never be set in a production scenario.
				var azureClient *http.Client
				azureClientRaw := cmd.Context().Value("azure-client")
				if azureClientRaw != nil {
					azureClient, _ = azureClientRaw.(*http.Client)
					if azureClient != nil {
						client.HTTPClient = azureClient
					}
				}
				exchangeToken = func(ctx context.Context) (codersdk.WorkspaceAgentAuthenticateResponse, error) {
					return client.AuthWorkspaceAzureInstanceIdentity(ctx)
				}
			}

			if exchangeToken != nil {
				// Agent's can start before resources are returned from the provisioner
				// daemon. If there are many resources being provisioned, this time
				// could be significant. This is arbitrarily set at an hour to prevent
				// tons of idle agents from pinging coderd.
				ctx, cancelFunc := context.WithTimeout(cmd.Context(), time.Hour)
				defer cancelFunc()
				for retry.New(100*time.Millisecond, 5*time.Second).Wait(ctx) {
					var response codersdk.WorkspaceAgentAuthenticateResponse

					response, err = exchangeToken(ctx)
					if err != nil {
						logger.Warn(ctx, "authenticate workspace", slog.F("method", auth), slog.Error(err))
						continue
					}
					client.SessionToken = response.SessionToken
					logger.Info(ctx, "authenticated", slog.F("method", auth))
					break
				}
				if err != nil {
					return xerrors.Errorf("agent failed to authenticate in time: %w", err)
				}
			}

			closer := agent.New(client.ListenWorkspaceAgent, &agent.Options{
				Logger: logger,
				EnvironmentVariables: map[string]string{
					// Override the "CODER_AGENT_TOKEN" variable in all
					// shells so "gitssh" works!
					"CODER_AGENT_TOKEN": client.SessionToken,
				},
			})
			<-cmd.Context().Done()
			return closer.Close()
		},
	}

	cliflag.StringVarP(cmd.Flags(), &auth, "auth", "", "CODER_AGENT_AUTH", "token", "Specify the authentication type to use for the agent")
	return cmd
}
