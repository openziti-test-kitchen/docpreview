package daemon

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/netfoundry/docpreview/internal/expose"
)

// adoptingExposer records what it was asked to adopt, reap and publish.
//
// Wraps the local exposer rather than reimplementing one: Publish has to produce a real
// Publication for recovery to carry on, and the interesting assertions are about which
// of the three calls happened for which key.
type adoptingExposer struct {
	expose.Exposer

	// offer is what this exposer claims to already have published.
	offer map[string]expose.Adoptable

	mu       sync.Mutex
	adopted  []string
	created  []string
	keptSets []map[string]bool

	// failAdopt makes Adopt refuse, to exercise the fall-back to Publish.
	failAdopt bool
}

func (a *adoptingExposer) Adoptable(context.Context) (map[string]expose.Adoptable, error) {
	return a.offer, nil
}

func (a *adoptingExposer) Adopt(ctx context.Context, spec expose.Spec, c expose.Adoptable,
	h http.Handler) (*expose.Publication, error) {

	if a.failAdopt {
		return nil, context.DeadlineExceeded
	}
	a.mu.Lock()
	a.adopted = append(a.adopted, spec.Key())
	a.mu.Unlock()
	return expose.NewPublication(c.Origin, spec.Name, func() error { return nil }), nil
}

func (a *adoptingExposer) Publish(ctx context.Context, spec expose.Spec,
	h http.Handler) (*expose.Publication, error) {

	a.mu.Lock()
	a.created = append(a.created, spec.Key())
	a.mu.Unlock()
	return a.Exposer.Publish(ctx, spec, h)
}

func (a *adoptingExposer) Reap(ctx context.Context, keep map[string]bool) error {
	a.mu.Lock()
	a.keptSets = append(a.keptSets, keep)
	a.mu.Unlock()
	return a.Exposer.Reap(ctx, keep)
}

func (a *adoptingExposer) took() ([]string, []string, []map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.adopted, a.created, a.keptSets
}

// TestPublishOrAdoptPrefersAdoption. Adoption binds a listener to a share that already
// exists; publishing is a controller round trip that answers in ten to fifteen seconds.
// Getting this the wrong way round is a restart that costs four minutes instead of ten
// seconds, and nothing in the logs would call it a fault.
func TestPublishOrAdoptPrefersAdoption(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	ex := &adoptingExposer{
		Exposer: d.exposer,
		offer:   map[string]expose.Adoptable{"abc123": {Handle: "tok", Origin: "https://x.example"}},
	}
	d.exposer = ex
	d.adoptable = ex.offer

	spec := expose.Spec{PreviewID: "abc123", Name: "docs-main", BaseURL: "/"}
	pub, adopted, err := d.publishOrAdopt(t.Context(), spec, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if !adopted {
		t.Fatal("a share that was already published was recreated instead of adopted")
	}
	if pub.URL != "https://x.example" {
		t.Errorf("URL = %q, want the origin the exposer already reported", pub.URL)
	}

	got, created, _ := ex.took()
	if len(got) != 1 || got[0] != "abc123" {
		t.Errorf("adopted %v, want [abc123]", got)
	}
	if len(created) != 0 {
		t.Errorf("also created %v", created)
	}
}

// TestAdoptionFallsBackToPublishing. A share that cannot be bound must not cost the
// preview: the fallback is the same call that would have run without adoption at all.
func TestAdoptionFallsBackToPublishing(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	ex := &adoptingExposer{
		Exposer:   d.exposer,
		offer:     map[string]expose.Adoptable{"abc123": {Handle: "tok", Origin: "https://x.example"}},
		failAdopt: true,
	}
	d.exposer = ex
	d.adoptable = ex.offer

	spec := expose.Spec{PreviewID: "abc123", Name: "docs-main", BaseURL: "/"}
	_, adopted, err := d.publishOrAdopt(t.Context(), spec, http.NotFoundHandler())
	if err != nil {
		t.Fatalf("a failed adoption was not retried as a publish: %v", err)
	}
	if adopted {
		t.Fatal("reported as adopted when Adopt refused")
	}
	if _, created, _ := ex.took(); len(created) != 1 {
		t.Errorf("created %v, want the one publication Adopt could not bind", created)
	}
}

// TestPublishOrAdoptCreatesWhatIsNotThere. The key is the whole test: a candidate under a
// different key must not be adopted for this spec, or a preview ends up serving another
// preview's share.
func TestPublishOrAdoptCreatesWhatIsNotThere(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	ex := &adoptingExposer{
		Exposer: d.exposer,
		offer:   map[string]expose.Adoptable{"someone-else": {Handle: "tok", Origin: "https://y.example"}},
	}
	d.exposer = ex
	d.adoptable = ex.offer

	spec := expose.Spec{PreviewID: "abc123", Name: "docs-main", BaseURL: "/"}
	_, adopted, err := d.publishOrAdopt(t.Context(), spec, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if adopted {
		t.Fatal("adopted a share belonging to a different publication key")
	}
	if got, created, _ := ex.took(); len(got) != 0 || len(created) != 1 {
		t.Errorf("adopted %v, created %v", got, created)
	}
}

// TestAdoptionIsSkippedByAnExposerThatCannotDoIt. Adopter is optional, and an exposer
// without it must keep working — `local` cannot adopt, because its URLs are paths on a
// listener that no longer exists.
func TestAdoptionIsSkippedByAnExposerThatCannotDoIt(t *testing.T) {
	_, d, _ := testIngress(t, &fakeClient{})

	// The stock local exposer, which implements no Adopter. A stale candidate map must
	// not tempt the daemon into a type assertion it cannot make.
	d.adoptable = map[string]expose.Adoptable{"abc123": {Handle: "tok", Origin: "https://x.example"}}

	spec := expose.Spec{PreviewID: "abc123", Name: "docs-main", BaseURL: "/"}
	_, adopted, err := d.publishOrAdopt(t.Context(), spec, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if adopted {
		t.Fatal("claimed to adopt through an exposer that does not implement Adopter")
	}
}
