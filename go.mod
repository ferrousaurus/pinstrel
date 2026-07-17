module pinstrel

go 1.26.5

// pinstrel pins the DAVE (Discord voice E2EE) capable fork of discordgo until
// PR #1704 ("Add E2EE (DAVE) Support") merges upstream. Without DAVE support,
// Discord voice servers reject the voice WS handshake with close code 4017
// ("E2EE/DAVE protocol required"), enforced globally since March 1st, 2026.
// The fork (yeongaori/dev, head commit of PR #1704) is pure-Go: it pulls in
// github.com/cloudflare/circl for MLS primitives, no new system/C deps.
// When PR #1704 lands upstream, delete this replace directive and run
// `go mod tidy` to resolve to the tagged release. See README "Maintenance".
replace github.com/bwmarrin/discordgo => github.com/yeongaori/discordgo v0.0.0-20260321152711-3d3293e4c765

require (
	github.com/bwmarrin/discordgo v0.29.1-0.20260214123928-f43dd94faaac
	github.com/pelletier/go-toml/v2 v2.4.3
	gopkg.in/hraban/opus.v2 v2.0.0-20230925203106-0188a62cb302
)

require (
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/gorilla/websocket v1.4.2 // indirect
	golang.org/x/crypto v0.32.0 // indirect
	golang.org/x/sys v0.29.0 // indirect
)
