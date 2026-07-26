// Package orgerr holds the sentinel errors an org load can fail with.
//
// It is a leaf package so the front router can classify a failure into an HTTP
// status without importing orgmanager (which would drag the whole tenant
// lifecycle into the router's test binary).
package orgerr

import "errors"

var (
	// ErrOrgNotFound means no org record matched the slug.
	ErrOrgNotFound = errors.New("org not found")

	// ErrOrgNotActive means the org exists but its status is not "active"
	// (provisioning, suspended, archived).
	//
	// This is deliberately NOT distinguished from ErrOrgNotFound in the HTTP
	// response: a distinct status code would tell an unauthenticated prober
	// which org slugs exist. The sentinel is for logs, not for clients.
	ErrOrgNotActive = errors.New("org not active")

	// ErrOrgUnavailable means the org exists and is active but its process is
	// not currently serving: spawn failed, the readiness handshake timed out,
	// or it is in crash-loop backoff. Unlike the other two this is transient,
	// so it maps to 503 + Retry-After rather than 404.
	ErrOrgUnavailable = errors.New("org unavailable")
)
