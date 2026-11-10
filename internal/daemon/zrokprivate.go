package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	httptransport "github.com/go-openapi/runtime/client"
	"github.com/openziti/zrok/v2/environment"
	"github.com/openziti/zrok/v2/environment/env_core"
	"github.com/openziti/zrok/v2/rest_client_zrok/metadata"
	"github.com/openziti/zrok/v2/sdk/golang/sdk"

	"github.com/netfoundry/docpreview/internal/config"
	"github.com/netfoundry/docpreview/internal/expose"
)

// zrokPrivateTarget is the target recorded on the admin share.
//
// It must not start with the zrok exposer's preview tag (`nfpreview:`). Reap deletes every share
// in this environment that carries that tag and that the database does not claim, and this share
// is not a preview.
const zrokPrivateTarget = "nfpreview-admin-listener"

// openZrokPrivate serves the daemon over a zrok private share.
//
// The zrok environment is the one the daemon already chose for its exposer (applyZrokScope runs
// before the listeners open), so the share belongs to the same account as the previews.
//
// A share that already holds the token in this environment is adopted, so a restart creates
// nothing. Otherwise one is created through the share gate, in closed permission mode: only the
// owning account and AccessGrants can access it.
func (ls *Listeners) openZrokPrivate(cfg config.ZrokPrivateListener, log *slog.Logger) (net.Listener, error) {
	root, err := environment.LoadRoot()
	if err != nil {
		return nil, fmt.Errorf("loading the zrok environment: %w", err)
	}
	if !root.IsEnabled() {
		return nil, fmt.Errorf("the zrok environment is not enabled, so there is no account to own " +
			"the private share; enable it from the dashboard or with `zrok2 enable`")
	}

	exists, err := zrokShareExists(root, cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("looking for the private share %s: %w", cfg.Token, err)
	}
	if exists {
		log.Info("adopting the existing zrok private share", "token", cfg.Token)
	} else {
		req := &sdk.ShareRequest{
			ShareMode:         sdk.PrivateShareMode,
			BackendMode:       sdk.ProxyBackendMode,
			Target:            zrokPrivateTarget,
			PrivateShareToken: cfg.Token,
			PermissionMode:    sdk.ClosedPermissionMode,
			AccessGrants:      cfg.AccessGrants,
		}
		create := func() error {
			return expose.DefaultShareGate().CreateShare(context.Background(), cfg.Token,
				expose.ShareReasonAdmin, func() error {
					_, cerr := sdk.CreateShare(root, req)
					return cerr
				})
		}
		if err := expose.RetryZrok(context.Background(), log, "create private share "+cfg.Token, create); err != nil {
			return nil, fmt.Errorf("creating the zrok private share %s (a token another account already "+
				"holds cannot be reused; pick another): %w", cfg.Token, err)
		}
		log.Info("created the zrok private share", "token", cfg.Token)
	}

	ln, err := sdk.NewListener(cfg.Token, root)
	if err != nil {
		return nil, fmt.Errorf("opening the zrok listener for private share %s: %w", cfg.Token, err)
	}
	return &overlayListener{Listener: ln, grantAll: true, label: "zrok private " + cfg.Token}, nil
}

// zrokShareExists reports whether this environment already holds a share with the token.
//
// Scoped to this environment, so a token held by another account or environment reads as absent
// and the create that follows fails with the controller's own reason.
func zrokShareExists(root env_core.Root, token string) (bool, error) {
	client, err := root.Client()
	if err != nil {
		return false, err
	}
	envZID := root.Environment().ZitiIdentity
	params := metadata.NewListSharesParams()
	params.EnvZID = &envZID
	resp, err := client.Metadata.ListShares(params,
		httptransport.APIKeyAuth("X-TOKEN", "header", root.Environment().AccountToken))
	if err != nil {
		return false, err
	}
	if resp.Payload == nil {
		return false, nil
	}
	for _, s := range resp.Payload.Shares {
		if s != nil && s.ShareToken == token {
			return true, nil
		}
	}
	return false, nil
}
