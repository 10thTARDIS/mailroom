// Neutral module path per the open-source portability rule (CLAUDE.md):
// no host that identifies a person or deployment. Renamed at publication time if needed.
module mailroom

go 1.26.0

require (
	github.com/ProtonMail/go-crypto v1.5.2
	github.com/anyproto/tantivy-go v1.0.6
	github.com/coreos/go-oidc/v3 v3.21.0
	github.com/emersion/go-imap/v2 v2.0.0-beta.8
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-msgauth v0.7.0
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/jackc/pgx/v5 v5.11.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/oklog/ulid/v2 v2.1.2
	github.com/pressly/goose/v3 v3.29.0
	github.com/prometheus/client_golang v1.25.0
	github.com/yuin/goldmark v1.8.6
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.62.0
	golang.org/x/oauth2 v0.37.0
	golang.org/x/sync v0.24.0
	golang.org/x/text v0.43.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sethvargo/go-retry v0.5.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)

// THE VENDORED go-imap FORK (#52, ADR-047 §5). Gmail's X-GM-EXT-1 FETCH items
// (X-GM-MSGID, X-GM-THRID, X-GM-LABELS) are a small patch on v2.0.0-beta.8,
// kept in-repo rather than in a hosted fork; what changed, why, and its
// upstream status are in third_party/go-imap/MAILROOM-PATCHES.md. Re-apply the
// patch on every go-imap bump, and drop this line once upstream has it.
replace github.com/emersion/go-imap/v2 => ./third_party/go-imap
