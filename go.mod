module tinycld.org/multi-org

go 1.26.3

// The fork is vendored in the tinycld repo (tinycld/third_party/pocketbase), which
// is its single source of truth — core and the app server resolve it from there
// too. The router must link the same copy: it shares core's libraries, and a
// second checkout would drift silently.
replace github.com/pocketbase/pocketbase => ../tinycld/third_party/pocketbase

replace tinycld.org/core => ../tinycld/core/server

// The pinned hosted feature menu (docs/SCOPE-tenant-feature-go.md, closed as
// option b): serve-org links these feature server modules so a tenant runs a
// feature's Go — through its RegisterTenant entry, gated by the org's resolved
// package set. The modules resolve from sibling checkouts, same shape as the
// core replace above; the menu itself lives in internal/tenantpkgs. Hosted
// packages OUTSIDE this menu are TS/rules-only.
replace tinycld.org/packages/calc => ../calc/server

replace tinycld.org/packages/calendar => ../calendar/server

replace tinycld.org/packages/contacts => ../contacts/server

replace tinycld.org/packages/drive => ../drive/server

replace tinycld.org/packages/mail => ../mail/server

replace tinycld.org/packages/text => ../text/server

require (
	github.com/evanw/esbuild v0.28.1
	github.com/google/uuid v1.6.0 // indirect
	github.com/grafana/sobek v0.0.0-20260722203707-64fef69693b6
	github.com/pocketbase/pocketbase v0.39.8
	golang.org/x/crypto v0.54.0
	golang.org/x/sync v0.22.0
	tinycld.org/core v0.0.0
)

require (
	github.com/Masterminds/semver/v3 v3.5.0
	github.com/emersion/go-smtp v0.24.0
	github.com/getsentry/sentry-go v0.44.1
	tinycld.org/packages/calc v0.0.0-00010101000000-000000000000
	tinycld.org/packages/calendar v0.0.0-00010101000000-000000000000
	tinycld.org/packages/contacts v0.0.0-00010101000000-000000000000
	tinycld.org/packages/drive v0.0.0-00010101000000-000000000000
	tinycld.org/packages/mail v0.0.0-00010101000000-000000000000
	tinycld.org/packages/text v0.0.0-00010101000000-000000000000
)

require (
	github.com/SherClockHolmes/webpush-go v1.4.0 // indirect
	github.com/adrg/strutil v0.2.2 // indirect
	github.com/adrg/sysfont v0.1.2 // indirect
	github.com/adrg/xdg v0.3.0 // indirect
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/asaskevich/govalidator v0.0.0-20230301143203-a9d515a09cc2 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/beevik/etree v1.6.0 // indirect
	github.com/benoitkugler/pstokenizer v1.0.1 // indirect
	github.com/benoitkugler/textlayout v0.3.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/clipperhouse/displaywidth v0.10.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.6.0 // indirect
	github.com/coder/websocket v1.8.14 // indirect
	github.com/disintegration/imaging v1.6.2 // indirect
	github.com/dlclark/regexp2/v2 v2.5.2 // indirect
	github.com/domodwyer/mailyak/v3 v3.6.2 // indirect
	github.com/dop251/base64dec v0.0.0-20231022112746-c6c9f9a96217 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/emersion/go-ical v0.0.0-20240127095438-fc1c9d8fb2b6 // indirect
	github.com/emersion/go-imap/v2 v2.0.0-beta.8 // indirect
	github.com/emersion/go-message v0.18.2 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/emersion/go-vcard v0.0.0-20260618161152-d854b7e0e2d3 // indirect
	github.com/emersion/go-webdav v0.7.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/ganigeorgiev/fexpr v0.5.0 // indirect
	github.com/go-sourcemap/sourcemap v2.1.4+incompatible // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/pprof v0.0.0-20260604005048-7023385849c0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jaytaylor/html2text v0.0.0-20260303211410-1a4bdc82ecec // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.23 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/microcosm-cc/bluemonday v1.0.27 // indirect
	github.com/mitchellh/copystructure v1.2.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/mrz1836/postmark v1.9.0 // indirect
	github.com/nathanstitt/doctaculous v0.1.0 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/olekukonko/cat v0.0.0-20250911104152-50322a0618f6 // indirect
	github.com/olekukonko/errors v1.2.0 // indirect
	github.com/olekukonko/ll v0.1.6 // indirect
	github.com/olekukonko/tablewriter v1.1.4 // indirect
	github.com/pocketbase/dbx v1.12.0 // indirect
	github.com/pocketbase/ozzo-validation/v4 v4.3.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/skyterra/y-crdt v0.0.0-20260224023949-c0cb10d3f33e // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/cobra v1.10.2 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/srwiley/rasterx v0.0.0-20220730225603-2ab79fcdd4ef // indirect
	github.com/ssor/bom v0.0.0-20170718123548-6386211fdfcf // indirect
	github.com/teambition/rrule-go v1.8.2 // indirect
	github.com/yuin/goldmark v1.8.2 // indirect
	golang.org/x/image v0.44.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	modernc.org/libc v1.74.1 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
	modernc.org/sqlite v1.54.0 // indirect
)
