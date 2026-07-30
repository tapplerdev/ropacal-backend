package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ropacal-backend/internal/middleware"
	"ropacal-backend/internal/orgdb"
	"ropacal-backend/internal/services/centrifugo"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// CentrifugoSubscribeRequest represents the subscribe proxy request from Centrifugo
type CentrifugoSubscribeRequest struct {
	ClientID  string                 `json:"client"`
	Transport string                 `json:"transport"`
	Protocol  string                 `json:"protocol"`
	Encoding  string                 `json:"encoding"`
	User      string                 `json:"user"`
	Channel   string                 `json:"channel"`
	Token     string                 `json:"token,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
	// Meta is the connection meta Centrifugo attached at connect time (from
	// the connection token's `meta` claim — see GenerateConnectionToken) and
	// forwards on every proxy call when the Centrifugo service is configured
	// with proxy_include_connection_meta. Carries {"org_id": "..."}.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// CentrifugoPublishRequest represents the publish proxy request from Centrifugo
type CentrifugoPublishRequest struct {
	ClientID  string                 `json:"client"`
	Transport string                 `json:"transport"`
	Protocol  string                 `json:"protocol"`
	Encoding  string                 `json:"encoding"`
	User      string                 `json:"user"`
	Channel   string                 `json:"channel"`
	Data      map[string]interface{} `json:"data"`
	// Meta: see CentrifugoSubscribeRequest.Meta.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// CentrifugoSubscribeResponse represents the subscribe proxy response
type CentrifugoSubscribeResponse struct {
	Result *CentrifugoSubscribeResult `json:"result,omitempty"`
	Error  *CentrifugoError           `json:"error,omitempty"`
}

// CentrifugoSubscribeResult represents successful subscription authorization
type CentrifugoSubscribeResult struct {
	// B64Data can be used to pass custom data to client
	B64Data string `json:"b64data,omitempty"`
	// ExpireAt is optional subscription expiration time (unix timestamp)
	ExpireAt int64 `json:"expire_at,omitempty"`
	// Info is optional JSON with subscription info
	Info map[string]interface{} `json:"info,omitempty"`
}

// CentrifugoPublishResponse represents the publish proxy response
type CentrifugoPublishResponse struct {
	Result *CentrifugoPublishResult `json:"result,omitempty"`
	Error  *CentrifugoError         `json:"error,omitempty"`
}

// CentrifugoPublishResult represents successful publication authorization
// Can include modified data to publish instead of original
type CentrifugoPublishResult struct {
	// Data is the modified data to publish (if we want to change it)
	Data map[string]interface{} `json:"data,omitempty"`
	// SkipHistory tells Centrifugo not to save this to channel history
	SkipHistory bool `json:"skip_history,omitempty"`
}

// CentrifugoError represents an error response
type CentrifugoError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// TENANCY: the proxy endpoints carry no user JWT (the caller is the
// Centrifugo SERVER), so their org context comes from the CONNECTION META —
// the {"org_id": ...} object GenerateConnectionToken minted into the client's
// connection token, which Centrifugo attaches server-side and forwards on
// every proxy call when configured with proxy_include_connection_meta (set
// together with the CENTRIFUGO_PROXY_SECRET static header; see CLAUDE.md).
//
//   - Tenancy dark (organizations table absent): meta is ignored and every
//     statement runs on the raw pool via a passthrough handle — byte-identical
//     to pre-tenancy behavior.
//   - Tenancy live + meta org present: ALL DB access for the request runs on
//     an org-bound handle (orgdb.System), so RLS sees the right tenant.
//   - Tenancy live + meta org absent + exactly ONE organization: resolve that
//     org (single-org grace, same rule as login). A connection predating the
//     flip, or a Centrifugo without proxy_include_connection_meta, forwards no
//     meta — and with one tenant the answer is unambiguous, so realtime keeps
//     working instead of dying for an external config change.
//   - Tenancy live + meta org absent + SEVERAL organizations: deny with a
//     Centrifugo-shaped error. There is no safe guess, and running the lookups
//     unscoped would silently return zero rows under RLS. The client
//     re-establishes with a fresh, org-bearing token via the normal lifecycle
//     (post-flip re-login / token refresh). This is the same threshold at
//     which CENTRIFUGO_PROXY_SECRET becomes mandatory.
//   - Tenancy live + meta org invalid: always deny.

// metaOrgID extracts org_id from a Centrifugo proxy request's connection
// meta. Returns "" when meta is absent, null, or not the expected shape.
func metaOrgID(meta json.RawMessage) string {
	if len(meta) == 0 {
		return ""
	}
	var m struct {
		OrgID string `json:"org_id"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return ""
	}
	return m.OrgID
}

// soleActiveOrgID returns the id of the ONLY active organization, or "" when
// there are zero or several. Cached briefly: proxy calls are frequent (driver
// GPS) and provisioning is rare, but the TTL means org #2 tightens this within
// seconds without a restart.
var (
	soleOrgMu sync.Mutex
	soleOrgID string
	soleOrgAt time.Time
)

func soleActiveOrgID(root *sqlx.DB) (string, error) {
	soleOrgMu.Lock()
	defer soleOrgMu.Unlock()
	if time.Since(soleOrgAt) < 30*time.Second {
		return soleOrgID, nil
	}
	var ids []string
	if err := root.Select(&ids, `SELECT id FROM organizations WHERE status = 'active' LIMIT 2`); err != nil {
		return "", err
	}
	soleOrgID = ""
	if len(ids) == 1 {
		soleOrgID = ids[0]
	}
	soleOrgAt = time.Now()
	return soleOrgID, nil
}

// proxyOrgDB resolves the DB handle a Centrifugo proxy request must use,
// per the TENANCY block above. ok=false means the request must be denied
// (tenancy is live but the connection carries no usable org).
func proxyOrgDB(root *sqlx.DB, meta json.RawMessage) (*orgdb.DB, bool) {
	if !orgdb.Migrated() {
		// Single-tenant dark mode: tokens carry no meta yet; raw-pool behavior.
		return orgdb.Passthrough(root), true
	}
	orgID := metaOrgID(meta)
	if orgID == "" {
		// SINGLE-ORG GRACE, mirroring login: a connection predating the flip (or
		// a Centrifugo not yet configured with proxy_include_connection_meta)
		// forwards no meta. While exactly ONE organization exists the answer is
		// unambiguous — it can only be that one — so resolve it rather than
		// killing realtime for a config change on an external service.
		// With two or more orgs there is no safe guess and we deny, which is
		// also when the proxy-secret guard becomes mandatory.
		only, err := soleActiveOrgID(root)
		if err != nil || only == "" {
			return nil, false
		}
		orgID = only
	}
	d, err := orgdb.System(root, orgID)
	if err != nil {
		log.Printf("🚫 [Centrifugo] Connection meta carried an invalid org id: %v", err)
		return nil, false
	}
	return d, true
}

// centrifugoOrgDenied is the shared Centrifugo-shaped error for proxy
// requests from connections without org context while tenancy is live.
var centrifugoOrgDenied = &CentrifugoError{
	Code:    403,
	Message: "organization context required (reconnect with a fresh token)",
}

// CentrifugoSubscribeProxy handles subscription authorization from Centrifugo
func CentrifugoSubscribeProxy(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CentrifugoSubscribeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [Centrifugo] Invalid subscribe request: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid request",
				},
			})
			return
		}

		// req.User is the identity Centrifugo resolved from the client's
		// authenticated connection token. It is trustworthy ONLY because the
		// CentrifugoProxyAuth middleware verified this request came from the
		// Centrifugo server itself (shared proxy secret) — never trust it on
		// an unguarded route.
		userID := req.User

		log.Printf("🔐 [Centrifugo] Subscribe request: user=%s channel=%s client=%s",
			userID, req.Channel, req.ClientID)

		// Tenant scope for every DB lookup below — from the connection meta.
		odb, ok := proxyOrgDB(db, req.Meta)
		if !ok {
			log.Printf("🚫 [Centrifugo] Subscribe denied — no org in connection meta (pre-flip connection?): user=%s channel=%s",
				userID, req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
				Error: centrifugoOrgDenied,
			})
			return
		}

		// Authorize subscription based on channel type
		authorized, err := authorizeSubscription(odb, userID, req.Channel)
		if err != nil {
			log.Printf("❌ [Centrifugo] Authorization error for user=%s channel=%s: %v",
				userID, req.Channel, err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "authorization failed",
				},
			})
			return
		}

		if !authorized {
			log.Printf("🚫 [Centrifugo] Subscription denied: user=%s channel=%s",
				userID, req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "permission denied",
				},
			})
			return
		}

		log.Printf("✅ [Centrifugo] Subscription authorized: user=%s channel=%s",
			userID, req.Channel)

		// Return successful authorization
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoSubscribeResponse{
			Result: &CentrifugoSubscribeResult{
				Info: map[string]interface{}{
					"user_id": userID,
					"channel": req.Channel,
				},
			},
		})
	}
}

// authorizeSubscription checks if a user is allowed to subscribe to a channel.
// db is org-bound when tenancy is live (a passthrough while dark), so under
// RLS the users/shifts lookups are automatically scoped to the caller's tenant.
// parsedChannel is the decomposed form of a Centrifugo channel name. There is
// exactly ONE parser (parseChannel) and every authorization path uses it.
//
// Channels come in two shapes, and the difference is NOT the segment count:
//
//	driver:location:{driverID}        namespace=driver kind=location id=<driverID>  org=""
//	company:{orgID}:events            namespace=company kind=events   id=""        org=<orgID>
//
// Both have three segments. Discriminating on len(parts) would therefore be
// wrong the moment a second channel family gains an org segment, so the org
// form is detected by trying to PARSE parts[1] as a UUID. That is also why the
// org sits in the middle: under the pre-D1 parser, company:events:{orgID} would
// have had parts[1] == "events" and been authorized for any admin of any org —
// a stale backend accepting it SILENTLY. With the org in the middle, parts[1] is
// a UUID, matches no legacy case, and falls through to a logged 403.
type parsedChannel struct {
	namespace string // driver, shift, manager, company
	kind      string // location, events, updates, notifications
	id        string // the trailing resource id, "" when the channel carries none
	orgID     string // set only for the org-scoped form
	legacy    bool   // true for the un-scoped company:events form (removed at D1 Deploy 3a)
}

func parseChannel(channel string) (parsedChannel, error) {
	parts := strings.Split(channel, ":")
	if len(parts) < 2 {
		return parsedChannel{}, fmt.Errorf("invalid channel format: %s", channel)
	}

	// Org-scoped form: <namespace>:<uuid>:<kind>
	if len(parts) == 3 {
		if _, err := uuid.Parse(parts[1]); err == nil {
			return parsedChannel{namespace: parts[0], kind: parts[2], orgID: parts[1]}, nil
		}
	}

	pc := parsedChannel{namespace: parts[0], kind: parts[1]}
	if len(parts) == 3 {
		pc.id = parts[2]
	}
	if pc.namespace == "company" && pc.kind == "events" && len(parts) == 2 {
		pc.legacy = true
	}
	return pc, nil
}

func authorizeSubscription(db *orgdb.DB, userID string, channel string) (bool, error) {
	ch, err := parseChannel(channel)
	if err != nil {
		return false, err
	}

	switch ch.namespace {
	case "driver":
		// driver:location:{driverId}
		if ch.kind == "location" && ch.id != "" {
			// Checks the target driver's org too — see canViewDriverLocation.
			return canViewDriverLocation(db, userID, ch.id)
		}
		// driver:events:{driverId} — only the driver themselves
		if ch.kind == "events" && ch.id != "" {
			return userID == ch.id, nil
		}

	case "shift":
		// shift:updates:{shiftId}
		if ch.kind == "updates" && ch.id != "" {
			return canViewShift(db, userID, ch.id)
		}

	case "manager":
		// manager:notifications:{managerId} — only that manager
		if ch.kind == "notifications" && ch.id != "" {
			return userID == ch.id, nil
		}

	case "company":
		// company:{orgID}:events — the org-scoped form. BOTH the org and the
		// role must match; an admin of another tenant is denied loudly.
		if ch.kind == "events" && ch.orgID != "" {
			if !sameOrgAsSubscriber(db, ch.orgID) {
				log.Printf("🚫 [Centrifugo] cross-org denial: subscriber org=%s tried company channel for org=%s",
					db.OrgID(), ch.orgID)
				return false, nil
			}
			return isCompanyEventViewer(db, userID)
		}
		// company:events — LEGACY, un-scoped, and the remaining isolation hole:
		// it is authorized on role alone, so any admin of any tenant receives
		// every tenant's operational feed. Kept only so clients can migrate;
		// D1 Deploy 3a deletes this branch, which is what actually closes the
		// hole. The log line makes remaining adoption observable.
		if ch.legacy {
			log.Printf("⚠️  [Centrifugo] LEGACY company:events subscribe by user=%s (org=%s) — migrate to company:{orgID}:events",
				userID, db.OrgID())
			return isCompanyEventViewer(db, userID)
		}
	}

	return false, fmt.Errorf("unknown channel format: %s (parsed ns=%q kind=%q id=%q org=%q legacy=%v)",
		channel, ch.namespace, ch.kind, ch.id, ch.orgID, ch.legacy)
}

// isCompanyEventViewer reports whether the user may receive the org-wide
// operational feed. Extracted so the scoped and legacy branches cannot drift
// apart while both exist.
func isCompanyEventViewer(db *orgdb.DB, userID string) (bool, error) {
	var role string
	err := db.Get(&role, `SELECT role FROM users WHERE id = $1`, userID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get user role: %w", err)
	}
	return role == "admin" || role == "manager", nil
}

// sameOrgAsSubscriber reports whether targetOrg belongs to the same tenant as the
// subscriber's org-bound handle.
//
// D2 (tenant isolation). RLS already scopes these lookups to the subscriber's org,
// so a foreign row normally comes back as ErrNoRows. This is a SECOND, explicit
// barrier for two reasons: it does not depend on RLS being correctly configured on
// every table forever, and it fails closed if a future query is ever run on an
// unscoped handle. `orgID == ""` means a passthrough handle (tenancy not migrated),
// where there is exactly one tenant and nothing to compare.
func sameOrgAsSubscriber(db *orgdb.DB, targetOrg string) bool {
	subscriberOrg := db.OrgID()
	if subscriberOrg == "" {
		return true // single-tenant / dark mode
	}
	return targetOrg == subscriberOrg
}

// canViewDriverLocation checks if user can view a driver's location.
//
// D2: this used to authorize ANY driverID once the subscriber was an admin — the
// driver's own org was never consulted. `db` is bound to the SUBSCRIBER's org, so
// the role lookup only proved "an admin of their own tenant", after which a
// foreign driver's UUID was accepted. Given a UUID, one tenant could live-track
// another tenant's fleet. The target is now resolved and its org compared.
func canViewDriverLocation(db *orgdb.DB, userID string, driverID string) (bool, error) {
	// Allow if user is the driver themselves
	if userID == driverID {
		return true, nil
	}

	// Resolve the TARGET driver first, on the subscriber's org-bound handle.
	// ErrNoRows here means the driver is foreign (RLS hid it) or nonexistent —
	// both are a denial, and deliberately indistinguishable to the caller.
	var target struct {
		Role  string `db:"role"`
		OrgID string `db:"organization_id"`
	}
	err := db.Get(&target, `SELECT role, organization_id FROM users WHERE id = $1`, driverID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to resolve target driver: %w", err)
	}
	if !sameOrgAsSubscriber(db, target.OrgID) {
		log.Printf("🚫 [Centrifugo] cross-org denial: subscriber org=%s tried driver %s (org=%s)",
			db.OrgID(), driverID, target.OrgID)
		return false, nil
	}

	// Then the subscriber's own role.
	var role string
	err = db.Get(&role, `SELECT role FROM users WHERE id = $1`, userID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get user role: %w", err)
	}

	// NOTE: "manager" is not a live role — the users CHECK constraint allows only
	// driver|admin. Kept for compatibility; admin is what actually matches.
	return role == "admin" || role == "manager", nil
}

// canViewShift checks if user can view shift updates.
//
// D2: identical hole to canViewDriverLocation, and worse in payload — the
// shift:updates channel carries the full task list (addresses, sequence, bins).
// A foreign admin holding a shift UUID received all of it.
func canViewShift(db *orgdb.DB, userID string, shiftID string) (bool, error) {
	// Resolve the shift and its owning org together.
	var shift struct {
		DriverID sql.NullString `db:"driver_id"`
		OrgID    string         `db:"organization_id"`
	}
	err := db.Get(&shift, `SELECT driver_id, organization_id FROM shifts WHERE id = $1`, shiftID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get shift: %w", err)
	}
	if !sameOrgAsSubscriber(db, shift.OrgID) {
		log.Printf("🚫 [Centrifugo] cross-org denial: subscriber org=%s tried shift %s (org=%s)",
			db.OrgID(), shiftID, shift.OrgID)
		return false, nil
	}

	// Allow if user is assigned to this shift
	if shift.DriverID.Valid && shift.DriverID.String == userID {
		return true, nil
	}

	// Check if user is a manager
	var role string
	err = db.Get(&role, `SELECT role FROM users WHERE id = $1`, userID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to get user role: %w", err)
	}

	return role == "admin" || role == "manager", nil
}

// CentrifugoPublishProxy handles publication authorization from Centrifugo
func CentrifugoPublishProxy(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req CentrifugoPublishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			log.Printf("❌ [Centrifugo] Invalid publish request: %v", err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    400,
					Message: "invalid request",
				},
			})
			return
		}

		// Same identity rule as the subscribe proxy: req.User comes from the
		// client's authenticated connection token and is trustworthy only
		// behind the CentrifugoProxyAuth shared-secret guard.
		userID := req.User

		log.Printf("🔐 [Centrifugo] Publish request: user=%s channel=%s client=%s",
			userID, req.Channel, req.ClientID)

		// Same org gate as the subscribe proxy. authorizePublication happens
		// to be DB-free today, but the contract is uniform: while tenancy is
		// live, no proxy request is served without tenant context.
		odb, ok := proxyOrgDB(db, req.Meta)
		if !ok {
			log.Printf("🚫 [Centrifugo] Publish denied — no org in connection meta (pre-flip connection?): user=%s channel=%s",
				userID, req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: centrifugoOrgDenied,
			})
			return
		}

		// Authorize publication based on channel type
		authorized, err := authorizePublication(odb, userID, req.Channel)
		if err != nil {
			log.Printf("❌ [Centrifugo] Authorization error for user=%s channel=%s: %v",
				userID, req.Channel, err)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "authorization failed",
				},
			})
			return
		}

		if !authorized {
			log.Printf("🚫 [Centrifugo] Publication denied: user=%s channel=%s",
				userID, req.Channel)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(CentrifugoPublishResponse{
				Error: &CentrifugoError{
					Code:    403,
					Message: "permission denied",
				},
			})
			return
		}

		log.Printf("✅ [Centrifugo] Publication authorized: user=%s channel=%s",
			userID, req.Channel)

		// Return successful authorization
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoPublishResponse{
			Result: &CentrifugoPublishResult{},
		})
	}
}

// authorizePublication checks if a user is allowed to publish to a channel.
// Takes the org-bound handle for signature uniformity with
// authorizeSubscription even though today's rules are DB-free.
func authorizePublication(db *orgdb.DB, userID string, channel string) (bool, error) {
	// Parse channel to determine type and resource ID
	parts := strings.Split(channel, ":")
	if len(parts) < 2 {
		return false, fmt.Errorf("invalid channel format: %s", channel)
	}

	namespace := parts[0]   // driver, shift, manager
	channelType := parts[1] // location, updates, notifications

	switch namespace {
	case "driver":
		// Channel format: driver:location:{driverId}
		if channelType == "location" && len(parts) == 3 {
			driverID := parts[2]
			// Only the driver themselves can publish to their own location channel
			return userID == driverID, nil
		}

	case "shift":
		// Drivers cannot publish to shift channels (backend only)
		return false, nil

	case "manager":
		// Drivers cannot publish to manager channels (backend only)
		return false, nil
	}

	return false, fmt.Errorf("unknown channel format: %s", channel)
}

// CentrifugoConnectionTokenResponse represents token response
type CentrifugoConnectionTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

// GetCentrifugoToken generates a connection token for the authenticated user
func GetCentrifugoToken(centrifugoClient *centrifugo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get user claims from context (set by auth middleware)
		userClaims, ok := middleware.GetUserFromContext(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		userID := userClaims.UserID

		// The caller's organization rides in the Centrifugo token's `meta`
		// claim so the proxy endpoints (server-to-server, no user JWT) can
		// re-establish tenant scope. Empty while tenancy is dark (the request
		// handle is a passthrough) → the claim is omitted and the token is
		// byte-identical to the pre-tenancy shape.
		orgID := orgdb.From(r).OrgID()

		// D4a: 1 hour, not 24. This token carries the org_id the proxy
		// endpoints trust for tenant scope, so its lifetime is how long a
		// revoked user keeps realtime access. Both SDKs auto-refresh off the
		// token's own exp (the dashboard never reads expires_at, Flutter uses
		// it only for logging), so shortening it needs no client change.
		//
		// 1 hour rather than minutes on purpose: the dashboard's refresh goes
		// through apiFetch, which carries the global 401 -> logout redirect, so
		// this TTL is also the interval at which the dashboard DISCOVERS app-JWT
		// expiry and hard-logs-out. At an hour that is harmless; at five
		// minutes it multiplies logout races and reconnect churn for no real
		// gain, since the subscribe proxy re-authorizes every new subscribe
		// regardless. Combined with D4b's 60s membership cache, realtime
		// revocation lands within roughly an hour, and minting a NEW connection
		// token is already blocked within 60s because this endpoint sits behind
		// Auth -> Org.
		expiresAt := time.Now().Add(1 * time.Hour)
		token, err := centrifugoClient.GenerateConnectionToken(userID, orgID, expiresAt)
		if err != nil {
			log.Printf("❌ [Centrifugo] Failed to generate token for user %s: %v", userID, err)
			http.Error(w, "failed to generate token", http.StatusInternalServerError)
			return
		}

		log.Printf("🔑 [Centrifugo] Generated connection token for user %s (expires: %s)",
			userID, expiresAt.Format(time.RFC3339))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CentrifugoConnectionTokenResponse{
			Token:     token,
			ExpiresAt: expiresAt.Unix(),
		})
	}
}
