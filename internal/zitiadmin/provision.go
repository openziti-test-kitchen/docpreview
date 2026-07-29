// Package zitiadmin provisions the OpenZiti objects docpreview needs from a
// controller's edge management API.
//
// It is the Go translation of scripts/ziti-trial/bootstrap.sh, with one
// difference that matters: the script deletes and recreates, this checks and
// creates. A script that starts by tearing down its own objects is fine for a
// throwaway trial and wrong for anything else — deleting the hosting identity
// invalidates the identity file docpreview is running from, and deleting a
// reviewer identity revokes a tunneler that was working a moment ago.
//
// Everything here goes through github.com/openziti/zrok/v2/controller/automation,
// which already wraps the edge-api calls and is a dependency either way. What
// it does not wrap is reading an enrollment JWT: its Enroll performs the
// enrollment itself and hands back a ziti.Config, which is what we want for
// docpreview's own identity and not what we want for a reviewer, who needs the
// token. See readEnrollmentJWT.
package zitiadmin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/michaelquigley/df/dl"
	"github.com/openziti/edge-api/rest_management_api_client/identity"
	"github.com/openziti/edge-api/rest_model"
	"github.com/openziti/sdk-golang/ziti"
	"github.com/openziti/zrok/v2/controller/automation"
)

// Options is everything `docpreview configure ziti` can be told.
type Options struct {
	// Controller is the edge management API root, e.g. "https://localhost:1280".
	Controller string

	// Username and Password authenticate against the controller's updb
	// authenticator — the admin account `ziti edge quickstart` creates.
	Username string
	Password string

	// Domain is the DNS suffix previews appear under. It ends up in the
	// intercept.v1 config and in the docpreview config file, and the two must
	// agree or the tunneler resolves names docpreview does not answer to.
	Domain string

	// Service is the wildcard service that carries every preview.
	Service string

	// AdminService, when set, is a second service for the dashboard and
	// webhook ingress. Empty means the ingress stays on TCP.
	AdminService string

	// HostIdentity is the name of docpreview's own hosting identity.
	HostIdentity string

	// Reviewer is the name of the sample reviewer identity whose enrollment
	// token gets imported into Ziti Desktop Edge.
	Reviewer string

	// ReaderRole is the attribute the Dial policy is keyed on. Adding another
	// reviewer is then one role attribute rather than a policy edit.
	ReaderRole string

	// Prefix is prepended to the derived object names (the config, the three
	// policies) so a second provisioning run against the same controller can
	// be told apart from the first.
	Prefix string

	// OutDir receives the enrolled identity file and the reviewer's .jwt.
	OutDir string
}

// Result reports what exists now and where the files landed.
type Result struct {
	HostIdentityFile string
	ReviewerJWTFile  string

	// ReviewerEnrolled is true when the reviewer identity has already consumed
	// its one-time token, in which case no .jwt could be written. Re-running
	// cannot fix that; a new token has to be minted deliberately.
	ReviewerEnrolled bool

	// Created and Reused name the controller objects, so the operator can see
	// that a second run changed nothing.
	Created []string
	Reused  []string
}

// Provision creates every object docpreview needs, skipping what is there.
func Provision(ctx context.Context, o Options, log *slog.Logger) (*Result, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	quietZrokLogging()

	za, err := automation.NewZitiAutomation(&automation.Config{
		ApiEndpoint: o.Controller,
		Username:    o.Username,
		Password:    o.Password,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to the ziti controller at %s "+
			"(is it running, and are the credentials right?): %w", o.Controller, err)
	}

	p := &provisioner{o: o, za: za, log: log, result: &Result{}}
	if err := p.run(ctx); err != nil {
		return nil, err
	}
	return p.result, nil
}

// quietZrokLogging stops the automation package narrating to stdout.
//
// It logs every create at info level through a global logger of its own, and
// that logger defaults to JSON on stdout when there is no terminal. The result
// is that `docpreview configure ziti` emits two records for every object — one
// zrok's, one ours — and that a script capturing stdout gets JSON it did not
// ask for. Warnings and errors still come through, because those are things
// the operator needs to see whoever wrote them.
func quietZrokLogging() {
	dl.Init(dl.DefaultOptions().SetLevel(slog.LevelWarn).SetOutput(os.Stderr))
}

func (o *Options) check() error {
	for name, value := range map[string]string{
		"controller":    o.Controller,
		"username":      o.Username,
		"domain":        o.Domain,
		"service":       o.Service,
		"host-identity": o.HostIdentity,
		"reader-role":   o.ReaderRole,
		"out-dir":       o.OutDir,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("-%s must not be empty", name)
		}
	}
	// A bare host:port would be parsed as a URL with no scheme, and the
	// resulting client would fail deep inside the CA fetch with an error that
	// mentions neither the flag nor the missing scheme.
	if !strings.HasPrefix(o.Controller, "http://") && !strings.HasPrefix(o.Controller, "https://") {
		o.Controller = "https://" + o.Controller
	}
	return nil
}

type provisioner struct {
	o      Options
	za     *automation.ZitiAutomation
	log    *slog.Logger
	result *Result
}

func (p *provisioner) run(ctx context.Context) error {
	if err := os.MkdirAll(p.o.OutDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", p.o.OutDir, err)
	}

	// Order matters in one place only: the service needs the config's id, and
	// the policies name the service. Everything else is independent.
	interceptID, err := p.ensureIntercept(p.o.Prefix+"intercept", []string{p.o.Domain, "*." + p.o.Domain})
	if err != nil {
		return err
	}
	serviceID, err := p.ensureService(p.o.Service, interceptID)
	if err != nil {
		return err
	}
	if err := p.ensurePolicies(p.o.Prefix, serviceID); err != nil {
		return err
	}

	// The admin service carries no intercept config. Nothing resolves a
	// hostname for it: docpreview's own SDK listener binds it and the
	// integration test dials it by name. A reviewer's tunneler has no reason
	// to reach the dashboard at all, so giving it a DNS entry would only widen
	// the surface.
	if p.o.AdminService != "" {
		adminID, err := p.ensureService(p.o.AdminService, "")
		if err != nil {
			return err
		}
		if err := p.ensurePolicies(p.o.Prefix+"admin-", adminID); err != nil {
			return err
		}
	}

	if err := p.ensureHostIdentity(); err != nil {
		return err
	}
	return p.ensureReviewer(ctx)
}

// ensureIntercept creates or updates the intercept.v1 config.
//
// Updated rather than left alone when it exists, because the addresses are
// derived from -domain: an operator who reruns with a different domain means
// to change it, and a stale config would have every tunneler resolving the old
// names.
//
// The apex is listed alongside the wildcard because the tunneler's matcher
// keys on the suffix — *.docpreview.ziti matches foo.docpreview.ziti but not
// the bare name.
func (p *provisioner) ensureIntercept(name string, addresses []string) (string, error) {
	data := map[string]any{
		"protocols":  []string{"tcp"},
		"addresses":  addresses,
		"portRanges": []map[string]int{{"low": 80, "high": 80}},
	}
	opts := &automation.ConfigOptions{
		BaseOptions:  automation.BaseOptions{Name: name},
		ConfigTypeID: interceptConfigTypeID,
		Data:         data,
	}

	existing, err := p.za.Configs.GetByName(name)
	if err != nil && !p.za.IsNotFound(err) {
		return "", fmt.Errorf("looking for the config %q: %w", name, err)
	}
	if existing != nil {
		if err := p.za.Configs.Update(*existing.ID, opts); err != nil {
			return "", fmt.Errorf("updating the config %q: %w", name, err)
		}
		p.reused("config " + name)
		return *existing.ID, nil
	}

	id, err := p.za.Configs.Create(opts)
	if err != nil {
		return "", fmt.Errorf("creating the config %q: %w", name, err)
	}
	p.created("config " + name)
	return id, nil
}

// interceptConfigTypeID is the well-known id of the intercept.v1 config type,
// fixed in ziti's own initial migration and therefore the same on every
// controller. Looked up by name would be more defensive; hardcoding it saves a
// round trip on a value that cannot change without breaking every tunneler.
const interceptConfigTypeID = "g7cIWbcGg"

func (p *provisioner) ensureService(name, configID string) (string, error) {
	existing, err := p.za.Services.GetByName(name)
	if err != nil && !p.za.IsNotFound(err) {
		return "", fmt.Errorf("looking for the service %q: %w", name, err)
	}
	if existing != nil {
		p.reused("service " + name)
		return *existing.ID, nil
	}

	opts := &automation.ServiceOptions{
		BaseOptions:        automation.BaseOptions{Name: name},
		EncryptionRequired: true,
	}
	if configID != "" {
		opts.Configs = []string{configID}
	}
	id, err := p.za.Services.Create(opts)
	if err != nil {
		return "", fmt.Errorf("creating the service %q: %w", name, err)
	}
	p.created("service " + name)
	return id, nil
}

// ensurePolicies creates the three policies that make a service usable: who
// may host it, who may reach it, and which routers carry it.
//
// Both service policies are keyed on role attributes rather than identity
// names. That is the difference between adding a reviewer being one attribute
// on a new identity and it being an edit to a live policy.
//
// serviceID, not the service name. The `@` role form is resolved against ids
// by the API — the `ziti` CLI's `@name` spelling is a client-side convenience
// it performs before the call, and passing a name here fails with "no services
// found with the given ids", which reads like the service does not exist.
func (p *provisioner) ensurePolicies(prefix, serviceID string) error {
	bind := &automation.ServicePolicyOptions{
		BaseOptions:   automation.BaseOptions{Name: prefix + "bind"},
		IdentityRoles: []string{"#" + p.o.HostIdentity},
		ServiceRoles:  []string{"@" + serviceID},
		Semantic:      rest_model.SemanticAllOf,
		PolicyType:    rest_model.DialBindBind,
	}
	dial := &automation.ServicePolicyOptions{
		BaseOptions:   automation.BaseOptions{Name: prefix + "dial"},
		IdentityRoles: []string{"#" + p.o.ReaderRole},
		ServiceRoles:  []string{"@" + serviceID},
		Semantic:      rest_model.SemanticAllOf,
		PolicyType:    rest_model.DialBindDial,
	}
	for _, policy := range []*automation.ServicePolicyOptions{bind, dial} {
		if err := p.ensureServicePolicy(policy); err != nil {
			return err
		}
	}

	name := prefix + "serp"
	existing, err := p.za.ServiceEdgeRouterPolicies.GetByName(name)
	if err != nil && !p.za.IsNotFound(err) {
		return fmt.Errorf("looking for the service edge router policy %q: %w", name, err)
	}
	if existing != nil {
		p.reused("service-edge-router-policy " + name)
		return nil
	}
	if _, err := p.za.ServiceEdgeRouterPolicies.Create(&automation.ServiceEdgeRouterPolicyOptions{
		BaseOptions:     automation.BaseOptions{Name: name},
		ServiceRoles:    []string{"@" + serviceID},
		EdgeRouterRoles: []string{"#all"},
		Semantic:        rest_model.SemanticAllOf,
	}); err != nil {
		return fmt.Errorf("creating the service edge router policy %q: %w", name, err)
	}
	p.created("service-edge-router-policy " + name)
	return nil
}

func (p *provisioner) ensureServicePolicy(opts *automation.ServicePolicyOptions) error {
	existing, err := p.za.ServicePolicies.GetByName(opts.Name)
	if err != nil && !p.za.IsNotFound(err) {
		return fmt.Errorf("looking for the service policy %q: %w", opts.Name, err)
	}
	if existing != nil {
		p.reused("service-policy " + opts.Name)
		return nil
	}
	if _, err := p.za.ServicePolicies.Create(opts); err != nil {
		return fmt.Errorf("creating the service policy %q: %w", opts.Name, err)
	}
	p.created("service-policy " + opts.Name)
	return nil
}

// ensureHostIdentity creates and enrolls docpreview's own identity.
//
// The identity and its file are one unit, and idempotence has to treat them
// that way. An enrollment token is one-time: once consumed, the only proof of
// that identity is the file this function wrote. So an identity on the
// controller that this file cannot authenticate as is not a state to preserve
// — docpreview could never host anything with it — and the honest repair is to
// replace it. That is the one destructive act in this package, and it is
// confined to docpreview's own identity.
//
// Two ways to end up there, and both are ordinary: the file was lost, or a
// previous run wrote a *different* file (a different -out-dir) and revoked
// this one. The second is why the file merely existing is not enough to trust
// it, and why the check is an actual authentication rather than a stat.
func (p *provisioner) ensureHostIdentity() error {
	path := filepath.Join(p.o.OutDir, p.o.HostIdentity+".json")
	p.result.HostIdentityFile = path

	existing, err := p.za.Identities.GetByName(p.o.HostIdentity)
	if err != nil && !p.za.IsNotFound(err) {
		return fmt.Errorf("looking for the identity %q: %w", p.o.HostIdentity, err)
	}

	if existing != nil {
		reason := identityFileUnusable(path)
		if reason == "" {
			p.reused("identity " + p.o.HostIdentity)
			return nil
		}
		p.log.Warn("recreating the hosting identity: its enrollment token is spent "+
			"and the identity file cannot be used",
			"identity", p.o.HostIdentity, "file", path, "reason", reason)

		if err := p.za.Identities.Delete(*existing.ID); err != nil {
			return fmt.Errorf("deleting the stale identity %q: %w", p.o.HostIdentity, err)
		}
	}

	id, err := p.za.Identities.Create(&automation.IdentityOptions{
		BaseOptions:    automation.BaseOptions{Name: p.o.HostIdentity},
		Type:           rest_model.IdentityTypeDefault,
		RoleAttributes: []string{p.o.HostIdentity},
	})
	if err != nil {
		return fmt.Errorf("creating the identity %q: %w", p.o.HostIdentity, err)
	}

	conf, err := p.za.Identities.Enroll(id)
	if err != nil {
		return fmt.Errorf("enrolling the identity %q: %w", p.o.HostIdentity, err)
	}
	body, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the enrolled identity: %w", err)
	}
	// 0600: this file is the private key that lets anything host docpreview's
	// services.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	p.created("identity " + p.o.HostIdentity)
	return nil
}

// identityFileUnusable reports why an identity file cannot be used, or "" if
// it can.
//
// It authenticates rather than checking that the file is present and parses.
// A file left over from an identity that has since been deleted and recreated
// looks perfect on disk and fails with a bare 401 the next time `serve` runs —
// which is a long way from the command that caused it. Catching it here is the
// difference between `configure ziti` repairing the setup and it cheerfully
// reporting that everything is already present.
func identityFileUnusable(path string) string {
	if _, err := os.Stat(path); err != nil {
		return "the file is missing"
	}
	zctx, err := ziti.NewContextFromFile(path)
	if err != nil {
		return "the file is not a valid identity: " + err.Error()
	}
	defer zctx.Close()

	if err := zctx.Authenticate(); err != nil {
		return "the controller rejected it: " + err.Error()
	}
	return ""
}

// ensureReviewer mints the sample reviewer identity and writes its token.
//
// Never destructive, unlike ensureHostIdentity: a reviewer identity with no
// local .jwt is the normal state after somebody imported the token into their
// tunneler, and deleting it would revoke a reviewer who is working fine.
func (p *provisioner) ensureReviewer(ctx context.Context) error {
	if p.o.Reviewer == "" {
		return nil
	}
	path := filepath.Join(p.o.OutDir, p.o.Reviewer+".jwt")
	p.result.ReviewerJWTFile = path

	existing, err := p.za.Identities.GetByName(p.o.Reviewer)
	if err != nil && !p.za.IsNotFound(err) {
		return fmt.Errorf("looking for the identity %q: %w", p.o.Reviewer, err)
	}

	id := ""
	if existing != nil {
		id = *existing.ID
		p.reused("identity " + p.o.Reviewer)
	} else {
		id, err = p.za.Identities.Create(&automation.IdentityOptions{
			BaseOptions:    automation.BaseOptions{Name: p.o.Reviewer},
			Type:           rest_model.IdentityTypeDefault,
			RoleAttributes: []string{p.o.ReaderRole},
		})
		if err != nil {
			return fmt.Errorf("creating the identity %q: %w", p.o.Reviewer, err)
		}
		p.created("identity " + p.o.Reviewer)
	}

	jwt, err := p.readEnrollmentJWT(ctx, id)
	if err != nil {
		return err
	}
	if jwt == "" {
		// The token was already consumed by a tunneler. Nothing to write, and
		// nothing broken.
		p.result.ReviewerEnrolled = true
		p.result.ReviewerJWTFile = ""
		return nil
	}
	if err := os.WriteFile(path, []byte(jwt), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// readEnrollmentJWT fetches an identity's outstanding one-time token.
//
// The detail read is not an optimization that could be skipped: the create
// response's payload carries only an id and _links, and the enrollment token
// appears solely on GET /identities/{id} as .enrollment.ott.jwt. The ziti CLI
// makes the same second call. Anyone who misses it concludes the API does not
// return tokens.
//
// An empty string means the identity has already enrolled, which is a normal
// state and not an error.
func (p *provisioner) readEnrollmentJWT(ctx context.Context, id string) (string, error) {
	params := &identity.DetailIdentityParams{Context: ctx, ID: id}
	params.SetTimeout(automation.DefaultOperationTimeout)

	resp, err := p.za.Edge().Identity.DetailIdentity(params, nil)
	if err != nil {
		return "", fmt.Errorf("reading identity %s: %w", id, err)
	}
	data := resp.GetPayload().Data
	if data == nil || data.Enrollment == nil || data.Enrollment.Ott == nil {
		return "", nil
	}
	return data.Enrollment.Ott.JWT, nil
}

func (p *provisioner) created(what string) {
	p.result.Created = append(p.result.Created, what)
	p.log.Info("created", "object", what)
}

func (p *provisioner) reused(what string) {
	p.result.Reused = append(p.result.Reused, what)
	p.log.Debug("already present", "object", what)
}
