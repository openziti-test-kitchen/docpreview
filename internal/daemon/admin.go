package daemon

import (
	"net/http"
)

// adminState is what the dashboard asks before deciding whether to offer the
// Secrets and Projects links.
//
// Two things it is not. It is not authentication: the answer only decides whether
// a link is drawn, and every write endpoint re-checks for itself, so a page that
// lies to itself gains nothing. And it is not a claim that the surfaces exist —
// each field is false both when the surface is unwired and when it is wired but
// would refuse this caller, because from the page's side those are the same fact.
type adminState struct {
	Secrets  bool   `json:"secrets"`
	Projects bool   `json:"projects"`
	Why      string `json:"why,omitempty"`
}

// admin answers whether this request could use the admin surfaces.
//
// Deliberately absent from the dashboard-only proxy's allowlist, which is the
// layer that matters: a tunnelled page's fetch 404s at the proxy and never reaches
// here. The isLocalRequest check below is the second layer, for the arrangement
// where somebody tunnels the daemon directly — there the request arrives with a
// forwarding header and this reports false, so the links stay hidden and the
// endpoints they point at refuse anyway.
//
// A Host-header test would be the wrong instrument. Host is whatever the client
// typed, so "localhost" proves nothing; where the connection came from does.
func (i *Ingress) admin(w http.ResponseWriter, r *http.Request) {
	local, why := isLocalRequest(r)

	state := adminState{Why: why}
	if local {
		if i.secrets != nil {
			if ok, unavailable := i.secrets.Available(); ok {
				state.Secrets = true
			} else {
				state.Why = unavailable
			}
		}
		if i.projects != nil {
			if ok, unavailable := i.projects.available(); ok {
				state.Projects = true
			} else if state.Why == "" {
				state.Why = unavailable
			}
		}
	}
	writeJSON(w, http.StatusOK, state)
}
