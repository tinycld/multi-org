package orgmanager

import (
	"net/http"
	"sync/atomic"

	"github.com/pocketbase/pocketbase"
)

// OrgInstance is a bootstrapped stock PocketBase app for one org plus its built
// HTTP mux. Instances are created lazily by the manager and evicted when idle.
type OrgInstance struct {
	slug     string
	app      *pocketbase.PocketBase
	mux      http.Handler
	lastUsed atomic.Int64 // unix nanos; seeded at load, updated on each Get (request dispatch)
}

func (i *OrgInstance) Mux() http.Handler { return i.mux }
func (i *OrgInstance) Slug() string      { return i.slug }

func (i *OrgInstance) touch(now int64) { i.lastUsed.Store(now) }

// close drains realtime clients and releases the app's DB/cron resources.
func (i *OrgInstance) close() error {
	// Drain SSE subscribers so ResetBootstrapState doesn't strand them.
	// Clients() returns a copy of the client map, so unregistering while
	// ranging is safe.
	if b := i.app.App.SubscriptionsBroker(); b != nil {
		for _, c := range b.Clients() {
			b.Unregister(c.Id())
		}
	}
	return i.app.App.ResetBootstrapState()
}
