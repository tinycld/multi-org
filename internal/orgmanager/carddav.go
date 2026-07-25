package orgmanager

import (
	"net/http"

	"github.com/pocketbase/pocketbase"

	"tinycld.org/core/carddav"
	"tinycld.org/multi-org/internal/lockfile"
)

// composeMux wraps the tenant's stock PocketBase mux with any host-side protocol
// handlers the org's packages declare. Today that's CardDAV (trusted Go over the
// tenant's own app/DB); the org is already fixed by the subdomain dispatch, so
// the handler uses the single-org scope and never resolves org itself.
//
// When no protocol handler applies, the stock mux is returned unchanged.
func (m *OrgManager) composeMux(pb *pocketbase.PocketBase, stockMux http.Handler, resolved []lockfile.ResolvedPackage) (http.Handler, error) {
	if m.cfg.CardDAVSources == nil {
		return stockMux, nil
	}
	sources, err := m.cfg.CardDAVSources(resolved)
	if err != nil {
		return nil, err
	}
	davHandler := carddav.HandlerFor(pb, sources)
	if davHandler == nil {
		return stockMux, nil
	}
	return &prefixMux{dav: davHandler, stock: stockMux}, nil
}

// prefixMux routes CardDAV URL prefixes to the CardDAV handler and everything
// else to the stock PocketBase mux. It is the per-org composition the front
// router dispatches to (inst.Mux()).
type prefixMux struct {
	dav   http.Handler
	stock http.Handler
}

func (p *prefixMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if carddav.HasPrefix(r.URL.Path) {
		p.dav.ServeHTTP(w, r)
		return
	}
	p.stock.ServeHTTP(w, r)
}
