package daemon

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/netfoundry/docpreview/internal/model"
)

// unsignedClient is a platform that accepts deliveries with no signature, which is what the
// local platform is until local.webhook_secret is set.
type unsignedClient struct {
	*fakeClient
	unsigned bool
}

func (u *unsignedClient) WebhookUnsigned() bool { return u.unsigned }

// An unsigned delivery is a build trigger with no authentication. It is accepted from this
// machine, typed by hand, and from nowhere else: not from another host, not through a proxy,
// and not from a web page the operator happens to have open, which can post to a loopback
// port as easily as curl can.
func TestAnUnsignedWebhookIsAcceptedOnlyFromThisMachineAndNotFromAPage(t *testing.T) {
	ingress, _, _ := testIngress(t, &fakeClient{})
	ingress.SetClient(model.PlatformLocal, &unsignedClient{fakeClient: &fakeClient{}, unsigned: true})
	h := ingress.Handler()

	send := func(remote string, headers map[string]string) int {
		r := httptest.NewRequest(http.MethodPost, "/webhook/local", bytes.NewReader([]byte(`{}`)))
		r.RemoteAddr = remote
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}

	for _, tc := range []struct {
		name    string
		remote  string
		headers map[string]string
		want    int
	}{
		{"curl on this machine", "127.0.0.1:50000", nil, http.StatusAccepted},
		{"curl on this machine over IPv6", "[::1]:50000", nil, http.StatusAccepted},
		{"curl with a content type", "127.0.0.1:50000",
			map[string]string{"Content-Type": "application/json"}, http.StatusAccepted},

		{"another host", "203.0.113.9:50000", nil, http.StatusForbidden},
		{"a LAN host", "192.168.1.20:50000", nil, http.StatusForbidden},
		{"through a proxy", "127.0.0.1:50000",
			map[string]string{"X-Forwarded-For": "203.0.113.9"}, http.StatusForbidden},
		{"through the public share", "127.0.0.1:50000",
			map[string]string{"X-Forwarded-Host": "abc.shares.zrok.io"}, http.StatusForbidden},

		// What a browser sends for a no-cors fetch or a form post from another site.
		{"a page on another site", "127.0.0.1:50000",
			map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
			http.StatusForbidden},
		{"a page on another loopback port", "127.0.0.1:50000",
			map[string]string{"Origin": "http://127.0.0.1:9000", "Sec-Fetch-Site": "same-site"},
			http.StatusForbidden},
		{"a page with only the fetch metadata", "127.0.0.1:50000",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"a sandboxed page", "127.0.0.1:50000",
			map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"the dashboard's own origin", "127.0.0.1:50000",
			map[string]string{"Origin": "http://127.0.0.1:8471", "Sec-Fetch-Site": "same-origin"},
			http.StatusForbidden},
	} {
		if got := send(tc.remote, tc.headers); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A platform whose deliveries are signed is not held to any of that: the signature is the
// authentication, and GitHub is not on this machine.
func TestASignedWebhookIsAcceptedFromAnywhere(t *testing.T) {
	ingress, _, _ := testIngress(t, &fakeClient{})
	ingress.SetClient(model.PlatformLocal, &unsignedClient{fakeClient: &fakeClient{}, unsigned: false})
	h := ingress.Handler()

	for _, route := range []string{"/webhook/local", "/webhook/github"} {
		r := httptest.NewRequest(http.MethodPost, route, bytes.NewReader([]byte(`{}`)))
		r.RemoteAddr = "203.0.113.9:50000"
		r.Header.Set("X-Forwarded-For", "198.51.100.7")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code == http.StatusForbidden {
			t.Errorf("%s: a signed platform's delivery was refused for where it came from", route)
		}
	}
}
