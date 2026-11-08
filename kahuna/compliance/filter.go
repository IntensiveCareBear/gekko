package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"gekko/kahuna/ingest"
)

// List of cryptographically hashed (SHA-256) authorized domains to prevent reverse engineering or modification of the safelist easily.
var authorizedDomainHashes = map[string]bool{
	// "api.kalshi.com"
	"75f189cebe0796b0ab1b18517b83c1146d28dd82717a83f95d084a38a099166f": true,
	// "clob.polymarket.com"
	"661a4d21277948b11082e9c0cb12c06368f8dbc7d68e8f630387b62dadcc230b": true,
	// "reuters.com"
	"f2f452a655ec9c4eb0fe36460542e115ff4baf01b06e0bc7e9c6b5ef916ea3cb": true,
	// "www.reuters.com"
	"66463dbff6b4b2bfdf3f0b4e2fc807a4cbe06ee85ffbf2441c57d9ddb901d6d5": true,
	// "twitter.com"
	"3c8b2bbd5af1180dab341c61a990008400c0f4734771d1618913d1a8fa55c929": true,
	// "x.com"
	"7f7ed12dfe7af503db4f7d8cfa1a264a4f9610fd6677b8ae297eb9dfafab279d": true,
	// "api.twitter.com"
	"1c181aebf815a6a40641ccdebe98c42276c2dc09cf077f55a90d9bde9cb215d5": true,
	// "mobile.twitter.com"
	"c301b2675216f4edd00a373f724c129fecf735336c61a359332c178b78df289e": true,
	// "news.google.com"
	"3d6774daa85546fa8dd16b497a1d0843829bd60ef60b8e7e98ba83f762d447d2": true,
	// "finnhub.io" — market-news aggregator; individual publisher domains aren't
	// allowlisted, so Finnhub events are attributed to finnhub.io (the source we pull
	// from) with the original article URL carried in the payload.
	"f1d69478800784c6d320b35900e1ec5f2f71b1a0787edce3c4e1221447573982": true,
	// "api.stlouisfed.org" — FRED official macro-data API (numeric ground truth for
	// the macro/econ markets); also serves the release-schedule calendar.
	"5e34d859d1d930449cfea9010173563f7f518712957f2030fb6f3fca581d5e94": true,
	// "www.reddit.com" — social/unverified (tier 4). We pull via oauth.reddit.com
	// but attribute emitted events to the post permalink on www.reddit.com.
	"582cff1d9cd20ceb2c0ce9f5585406104fbf87bc6598643842862f781f181e40": true,
	// "reddit.com" — bare-domain permalinks (some API responses omit the www).
	"90ce957778d13b453b7416c943e1720b3379d7c77814c6b70028cbbf7aecb644": true,
	// "aviationweather.gov" — NOAA Aviation Weather Center METAR API (keyless numeric
	// ground truth for Kalshi daily high-temperature markets).
	"ed9e2416099fbb5696e97d2020f11bce4a946e6f8ce6ff35ff495c8459f70fd1": true,
	// "api.weather.gov" — NWS official forecast API (keyless daily-high forecast that
	// feeds the Phase 2 weather forecast-bias model).
	"148991c618fcf1097ec1bf4b034903706b91722924ddf9c91d67c0c5f7f790dc": true,
}

type Gatekeeper struct {
	privatePattern *regexp.Regexp
}

func NewGatekeeper() *Gatekeeper {
	// Rejects telegram links/webhooks, discord webhooks/invites, slack webhooks, whatsapp groups, and private chat traces
	pattern := `(?i)(discord\.com/api/webhooks/|discordapp\.com/api/webhooks/|api\.telegram\.org/bot|t\.me/joinchat|hooks\.slack\.com/services/|chat\.whatsapp\.com/|discord\.gg/|t\.me/[a-zA-Z0-9_]{5,})`
	return &Gatekeeper{
		privatePattern: regexp.MustCompile(pattern),
	}
}

// Validate checks the event's SourceURL, RawContent, and PayloadHash for compliance.
func (g *Gatekeeper) Validate(event *ingest.GekkoIngestEvent) (bool, error) {
	if event == nil {
		return false, errors.New("nil event")
	}

	// 1. Verify Payload Hash integrity
	computedHash := event.ComputeHash()
	if event.PayloadHash != computedHash {
		return false, fmt.Errorf("cryptographic mismatch: computed payload hash %s does not match provided hash %s", computedHash, event.PayloadHash)
	}

	// 2. Scan RawContent and SourceURL for private webhook/chat signatures
	if g.privatePattern.MatchString(event.RawContent) {
		return false, fmt.Errorf("unauthorized private chat/webhook signature found in RawContent")
	}
	if g.privatePattern.MatchString(event.SourceURL) {
		return false, fmt.Errorf("unauthorized private chat/webhook signature found in SourceURL")
	}

	// 3. Parse and check the SourceURL domain
	parsedURL, err := url.Parse(event.SourceURL)
	if err != nil {
		return false, fmt.Errorf("failed to parse SourceURL: %w", err)
	}

	host := parsedURL.Hostname()
	if host == "" {
		return false, errors.New("empty host in SourceURL")
	}

	// Clean host (remove trailing dot if any, convert to lowercase)
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	// 4. Block loopback, private IP ranges, and local top-level domains
	if isPrivateOrLoopbackHost(host) {
		return false, fmt.Errorf("unauthorized access to private/loopback address: %s", host)
	}

	// 5. Cryptographic domain verification
	// We hash the host using SHA-256 and verify it exists in the authorizedDomainHashes map.
	// Since subdomains are sometimes dynamic, we check the exact host first,
	// then we also verify base domains for subdomains (e.g. if the host is market-updates.reuters.com,
	// we want to authorize reuters.com).
	if !g.isDomainAuthorized(host) {
		return false, fmt.Errorf("unauthorized source domain: %s (cryptographic check failed)", host)
	}

	return true, nil
}

func (g *Gatekeeper) isDomainAuthorized(host string) bool {
	// Only exact matches against the allowlist are accepted.
	// Subdomain suffix fallbacks are intentionally absent — they allowed any
	// *.reuters.com or *.polymarket.com host to pass, including attacker-
	// controlled subdomains.
	return hashAndCheck(host)
}

func hashAndCheck(domain string) bool {
	h := sha256.Sum256([]byte(domain))
	hexHash := hex.EncodeToString(h[:])
	return authorizedDomainHashes[hexHash]
}

func isPrivateOrLoopbackHost(host string) bool {
	// Check if the host name is direct localhost or local TLDs
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") || strings.HasSuffix(host, ".onion") {
		return true
	}

	// Check if it parses as an IP address
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsPrivate() {
			return true
		}
	}

	// If the host is in CIDR ranges manually checked (e.g., standard private subnets)
	// (Go's ip.IsPrivate() covers RFC 1918 and RFC 4193, but we can do extra checks if needed)
	return false
}
