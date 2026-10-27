package management

// Buff Cloud fork: the endpoints Buff Cloud's AI page reads and writes, under
// /v8/management/buff. See docs/buff-cloud/api.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/linked"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buffcloud"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var (
	buffStartedAt  = time.Now()
	buffCodexProbe = buffcloud.NewCodexUsageProbe()
)

// buffStrategies are the routing strategies Buff Cloud may set, with the
// aliases upstream accepts for its own.
var buffStrategies = map[string]string{
	coreauth.RoomStrategyMostRoom:     coreauth.RoomStrategyMostRoom,
	coreauth.RoomStrategySoonestReset: coreauth.RoomStrategySoonestReset,
	"round-robin":                     "round-robin",
	"roundrobin":                      "round-robin",
	"rr":                              "round-robin",
	"weighted-round-robin":            "weighted-round-robin",
	"weightedroundrobin":              "weighted-round-robin",
	"wrr":                             "weighted-round-robin",
	"fill-first":                      "fill-first",
	"fillfirst":                       "fill-first",
	"ff":                              "fill-first",
}

func normalizeBuffStrategy(strategy string) (string, bool) {
	s, ok := buffStrategies[strings.ToLower(strings.TrimSpace(strategy))]
	return s, ok
}

type buffSettings struct {
	Strategy           string  `json:"strategy"`
	Pinned             *string `json:"pinned"`
	PinnedFallback     bool    `json:"pinnedFallback"`
	SessionAffinity    bool    `json:"sessionAffinity"`
	SessionAffinityTTL string  `json:"sessionAffinityTTL"`
	ProxyEnabled       bool    `json:"proxyEnabled"`
	// Websocket is whether any Codex credential uses the WebSocket transport.
	Websocket bool `json:"websocket"`
}

type buffService struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	StartedAt int64  `json:"startedAt"`
	// Features names the additions a client may rely on; see docs/buff-cloud/usage-contract.md.
	Features []string `json:"features"`
}

// buffFeatures are this build's feature flags, in the order they were added.
var buffFeatures = []string{"usage-range", "usage-fields", "usage-client", "usage-reserve"}

type buffCodex struct {
	// Signals are the x-codex-* headers of the account's last response.
	Signals map[string]string     `json:"signals"`
	Usage   *buffcloud.CodexUsage `json:"usage,omitempty"`
}

type buffAccount struct {
	buffcloud.Account
	Email          *string    `json:"email"`
	LinkedDir      string     `json:"linkedDir,omitempty"`
	Status         string     `json:"status"`
	StatusMessage  string     `json:"statusMessage,omitempty"`
	ObservedAt     *int64     `json:"observedAt"`
	LastUsedAt     *int64     `json:"lastUsedAt"`
	TokenExpiresAt *int64     `json:"tokenExpiresAt"`
	Websockets     *bool      `json:"websockets,omitempty"`
	Codex          *buffCodex `json:"codex,omitempty"`
	// Reserve is the share of each window kept free; null when none is set.
	Reserve *coreauth.Reserve `json:"reserve"`
	// Held lists the windows at their reserve now; the router skips the account while any is.
	Held []buffReserveHold `json:"held"`
}

type buffReserveHold struct {
	Window  string  `json:"window"`
	Used    float64 `json:"used"`
	Reserve float64 `json:"reserve"`
	Until   *int64  `json:"until"`
}

// BuffAccounts serves GET /v8/management/buff/accounts.
func (h *Handler) BuffAccounts(c *gin.Context) {
	now := time.Now()
	accounts := h.buffAccounts(c.Request.Context(), now, true)
	c.JSON(http.StatusOK, gin.H{
		"settings": h.buffSettings(),
		"service":  buffServiceInfo(),
		"accounts": accounts,
	})
}

// BuffUsage serves GET /v8/management/buff/usage?since=<epoch ms>; since
// defaults to 24 hours ago. With only since it answers {since, rows}, every
// routed row oldest first, as it always has. The newer parameters (until,
// filters, status, unrouted, limit, cursor) add until and next to the reply.
func (h *Handler) BuffUsage(c *gin.Context) {
	now := time.Now()
	values := c.Request.URL.Query()
	since, ok := buffQueryMs(c, "since", now.Add(-24*time.Hour).UnixMilli())
	if !ok {
		return
	}
	if !buffcloud.HasUsageQueryParams(values) {
		rows, err := buffUsageQuery(buffcloud.UsageQuery{Since: since})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"since": since, "rows": rows})
		return
	}
	until, ok := buffQueryMs(c, "until", now.UnixMilli())
	if !ok {
		return
	}
	filter, err := buffcloud.ParseUsageFilter(values)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := buffcloud.UsageQuery{Since: since, Until: until, Filter: filter, Cursor: strings.TrimSpace(values.Get("cursor"))}
	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		n, errAtoi := strconv.Atoi(raw)
		if errAtoi != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive number"})
			return
		}
		q.Limit = min(n, buffcloud.UsageLimitMax)
	}
	if q.Cursor != "" && q.Limit == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cursor needs limit"})
		return
	}
	reply := gin.H{"since": since, "until": until}
	var next string
	l := buffcloud.DefaultUsageLog()
	rows := []buffcloud.Row{}
	if l != nil {
		var found []buffcloud.Row
		found, next, err = l.Query(q)
		if errors.Is(err, buffcloud.ErrUsageCursor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if found != nil {
			rows = found
		}
	}
	reply["rows"] = rows
	if next != "" {
		reply["next"] = next
	}
	c.JSON(http.StatusOK, reply)
}

// BuffUsageSeries serves GET /v8/management/buff/usage/series: the filtered
// rows counted into time buckets and groups. since defaults to 24 hours ago,
// until to now, bucket to hour, groupBy to none and tz to the box's zone.
func (h *Handler) BuffUsageSeries(c *gin.Context) {
	now := time.Now()
	values := c.Request.URL.Query()
	since, ok := buffQueryMs(c, "since", now.Add(-24*time.Hour).UnixMilli())
	if !ok {
		return
	}
	until, ok := buffQueryMs(c, "until", now.UnixMilli())
	if !ok {
		return
	}
	filter, err := buffcloud.ParseUsageFilter(values)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	q := buffcloud.SeriesQuery{
		Since: since, Until: until, Filter: filter,
		Bucket: strings.TrimSpace(values.Get("bucket")), GroupBy: strings.TrimSpace(values.Get("groupBy")),
		Loc: time.Local,
	}
	if q.Bucket == "" {
		q.Bucket = "hour"
	}
	if q.GroupBy == "" {
		q.GroupBy = "none"
	}
	tz := buffcloud.LocalZoneName()
	if raw := strings.TrimSpace(values.Get("tz")); raw != "" {
		loc, errLoad := time.LoadLocation(raw)
		if errLoad != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tz must be an IANA time zone such as Europe/London"})
			return
		}
		q.Loc, tz = loc, raw
	}
	if err := q.Validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	l := buffcloud.DefaultUsageLog()
	if l == nil {
		c.JSON(http.StatusOK, buffcloud.Series{Since: since, Until: until, Bucket: q.Bucket, GroupBy: q.GroupBy, TZ: tz, Buckets: []buffcloud.SeriesBucket{}})
		return
	}
	series, err := l.Series(q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	series.TZ = tz
	c.JSON(http.StatusOK, series)
}

// buffQueryMs reads an epoch-ms query parameter, or answers 400.
func buffQueryMs(c *gin.Context, name string, fallback int64) (int64, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, true
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": name + " must be epoch milliseconds"})
		return 0, false
	}
	return parsed, true
}

// BuffSummary serves GET /v8/management/buff/summary?now=<epoch ms>&dayStart=<epoch ms>;
// now defaults to the current time and dayStart to the local midnight before now.
func (h *Handler) BuffSummary(c *gin.Context) {
	now := time.Now()
	if raw := strings.TrimSpace(c.Query("now")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "now must be epoch milliseconds"})
			return
		}
		now = time.UnixMilli(parsed)
	}
	y, m, d := now.Date()
	dayStart := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	if raw := strings.TrimSpace(c.Query("dayStart")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "dayStart must be epoch milliseconds"})
			return
		}
		dayStart = time.UnixMilli(parsed)
	}
	since := dayStart
	if hour := now.Add(-time.Hour); hour.Before(since) {
		since = hour
	}
	rows, err := buffUsageQuery(buffcloud.UsageQuery{Since: since.UnixMilli()})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	full := h.buffAccounts(c.Request.Context(), now, false)
	accounts := make([]buffcloud.Account, len(full))
	for i, a := range full {
		accounts[i] = a.Account
	}
	// Rows stamped after now belong to a later summary.
	kept := rows[:0]
	for _, row := range rows {
		if row.At <= now.UnixMilli() {
			kept = append(kept, row)
		}
	}
	c.JSON(http.StatusOK, buffcloud.Summarise(accounts, kept, now, dayStart))
}

// buffUsageQuery is the routed rows of q, oldest first; rows with no account
// are left out unless q asks for them.
func buffUsageQuery(q buffcloud.UsageQuery) ([]buffcloud.Row, error) {
	l := buffcloud.DefaultUsageLog()
	if l == nil {
		return []buffcloud.Row{}, nil
	}
	rows, _, err := l.Query(q)
	if rows == nil {
		rows = []buffcloud.Row{}
	}
	return rows, err
}

// PatchBuffSettings serves PATCH /v8/management/buff/settings. Each field is
// optional: strategy, pinned (a credential id, or null or "" to clear),
// pinnedFallback, proxyEnabled. Nothing is written unless every field is valid.
func (h *Handler) PatchBuffSettings(c *gin.Context) {
	var body map[string]json.RawMessage
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must be a JSON object"})
		return
	}
	var patch buffSettingsPatch
	if err := patch.parse(body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if patch.pinned != nil && h.authManager != nil {
		if err := validateBuffPin(*patch.pinned, h.authManager.List()); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	h.mu.Lock()
	patch.apply(&h.cfg.Routing)
	snapshot, ok := h.saveConfigAndSnapshotLocked(c)
	h.mu.Unlock()
	if !ok {
		return
	}
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, h.buffSettings())
}

// PatchBuffReserve serves PATCH /v8/management/buff/reserve with
// {"id": "<credential id>", "reserve": {"all": 0.2, "windows": {"7d": 0.3}}},
// or "reserve": null to clear it. The reserve is written into the credential's
// auth file, as Enabled is, and the answer is the account as /buff/accounts
// lists it.
func (h *Handler) PatchBuffReserve(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var body struct {
		ID      string          `json:"id"`
		Reserve json.RawMessage `json:"reserve"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.ID) == "" || len(body.Reserve) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": `body must be {"id": "<credential id>", "reserve": {"all": 0.2} or null}`})
		return
	}
	id := strings.TrimSpace(body.ID)
	var reserve *coreauth.Reserve
	if string(body.Reserve) != "null" {
		parsed, err := coreauth.ParseReserve(body.Reserve)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !parsed.IsZero() {
			reserve = &parsed
		}
	}
	target, ok := h.authManager.GetByID(id)
	if !ok || target == nil || !isAuthFileListable(target) || target.AuthSourceKind() == coreauth.AuthSourceConfig {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no credential %q; use an id from GET /v8/management/buff/accounts", id)})
		return
	}
	if coreauth.IsPluginVirtualAuth(target) {
		c.JSON(http.StatusConflict, gin.H{"error": errPluginVirtualAuth.Error()})
		return
	}
	ctx := c.Request.Context()
	updated, committed, err := h.authManager.Modify(ctx, target.ID, target.RegistrationEpoch, func(current *coreauth.Auth) error {
		if current.Metadata == nil {
			current.Metadata = make(map[string]any)
		}
		if reserve == nil {
			delete(current.Metadata, coreauth.ReserveMetadataKey)
			return nil
		}
		windows := make(map[string]any, len(reserve.Windows))
		for window, v := range reserve.Windows {
			windows[window] = v
		}
		value := map[string]any{"all": reserve.All}
		if len(windows) > 0 {
			value["windows"] = windows
		}
		current.Metadata[coreauth.ReserveMetadataKey] = value
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save the reserve: %v", err)})
		return
	}
	if !committed || updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("no credential %q", id)})
		return
	}
	if h.postAuthPersistHook != nil {
		if errHook := h.postAuthPersistHook(ctx, updated); errHook != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("post-auth persist hook failed: %v", errHook)})
			return
		}
	}
	for _, account := range h.buffAccounts(ctx, time.Now(), false) {
		if account.ID == updated.ID {
			c.JSON(http.StatusOK, account)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"id": updated.ID})
}

type buffSettingsPatch struct {
	strategy       *string
	pinned         *string
	pinnedFallback *bool
	proxyEnabled   *bool
}

func (p *buffSettingsPatch) parse(body map[string]json.RawMessage) error {
	for key, raw := range body {
		switch key {
		case "strategy":
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return fmt.Errorf("strategy must be a string")
			}
			normalized, ok := normalizeBuffStrategy(s)
			if !ok {
				return fmt.Errorf("unknown strategy %q; use most-room, soonest-reset, round-robin, weighted-round-robin or fill-first", s)
			}
			p.strategy = &normalized
		case "pinned":
			pinned := ""
			if string(raw) != "null" && json.Unmarshal(raw, &pinned) != nil {
				return fmt.Errorf("pinned must be a credential id, null or \"\"")
			}
			pinned = strings.TrimSpace(pinned)
			p.pinned = &pinned
		case "pinnedFallback", "proxyEnabled":
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				return fmt.Errorf("%s must be true or false", key)
			}
			if key == "pinnedFallback" {
				p.pinnedFallback = &b
			} else {
				p.proxyEnabled = &b
			}
		default:
			return fmt.Errorf("unknown setting %q", key)
		}
	}
	return nil
}

func (p buffSettingsPatch) apply(r *config.RoutingConfig) {
	if p.strategy != nil {
		r.Strategy = *p.strategy
	}
	if p.pinned != nil {
		r.Pinned = *p.pinned
	}
	if p.pinnedFallback != nil {
		fallback := *p.pinnedFallback
		r.PinnedFallback = &fallback
	}
	if p.proxyEnabled != nil {
		r.Paused = !*p.proxyEnabled
	}
}

// validateBuffPin accepts "" (no pin) or the id of an enabled credential. The
// router ignores a pin that names no credential, so a typo would silently
// leave routing unpinned.
func validateBuffPin(id string, auths []*coreauth.Auth) error {
	if id == "" {
		return nil
	}
	for _, auth := range auths {
		if auth == nil || auth.ID != id {
			continue
		}
		if auth.Disabled || auth.Status == coreauth.StatusDisabled {
			return fmt.Errorf("credential %q is disabled", id)
		}
		return nil
	}
	return fmt.Errorf("no credential %q; use an id from GET /v8/management/buff/accounts", id)
}

func (h *Handler) buffSettings() buffSettings {
	h.mu.Lock()
	routing := h.cfg.Routing
	h.mu.Unlock()
	strategy, ok := normalizeBuffStrategy(routing.Strategy)
	if !ok {
		strategy = "round-robin"
	}
	settings := buffSettings{
		Strategy:           strategy,
		PinnedFallback:     routing.PinnedFallback == nil || *routing.PinnedFallback,
		SessionAffinity:    routing.SessionAffinity,
		SessionAffinityTTL: routing.SessionAffinityTTL,
		ProxyEnabled:       !routing.Paused,
	}
	if pinned := strings.TrimSpace(routing.Pinned); pinned != "" {
		settings.Pinned = &pinned
	}
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if ws, ok := authWebsocketsValue(auth); ok && ws && strings.EqualFold(auth.Provider, "codex") {
				settings.Websocket = true
			}
		}
	}
	return settings
}

func buffServiceInfo() buffService {
	commit := buildinfo.Commit
	if commit == "" || commit == "none" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" {
					commit = s.Value
				}
			}
		}
	}
	return buffService{
		Version: buildinfo.Version, Commit: commit, BuildDate: buildinfo.BuildDate, StartedAt: buffStartedAt.UnixMilli(),
		Features: append([]string{}, buffFeatures...),
	}
}

// buffAccounts lists every credential, sorted by id. withCodexUsage adds the
// wham/usage probe for Codex credentials.
func (h *Handler) buffAccounts(ctx context.Context, now time.Time, withCodexUsage bool) []buffAccount {
	accounts := []buffAccount{}
	if h.authManager == nil {
		return accounts
	}
	usageLog := buffcloud.DefaultUsageLog()
	for _, auth := range h.authManager.List() {
		if !isAuthFileListable(auth) {
			continue
		}
		a := buffAccount{
			Account: buffcloud.Account{
				ID:       auth.ID,
				Provider: buffcloud.ProviderName(auth.Provider),
				Label:    auth.Label,
				Enabled:  !auth.Disabled && auth.Status != coreauth.StatusDisabled,
				Rooms:    []buffcloud.Room{},
			},
			LinkedDir: linked.Dir(auth.Metadata),
		}
		if email := authEmail(auth); email != "" {
			a.Email = &email
		}
		if plan, _ := auth.Metadata["plan_type"].(string); strings.TrimSpace(plan) != "" {
			plan = strings.TrimSpace(plan)
			a.Plan = &plan
		}
		for _, room := range coreauth.RoomsFromQuota(auth.Provider, auth.Quota) {
			a.Rooms = append(a.Rooms, buffcloud.Room{Window: room.Window, Used: room.Used, ResetsAt: epochMs(room.ResetsAt)})
		}
		a.ObservedAt = epochMs(auth.Quota.ObservedAt)
		unavailable, status, message, nextRetry := reconcileAuthFileCooldownState(auth, now)
		a.Status, a.StatusMessage = string(status), message
		if unavailable && nextRetry.After(now) {
			a.CooldownUntil = epochMs(nextRetry)
		}
		if usageLog != nil {
			if last, ok := usageLog.Last(auth.ID); ok {
				status, at := last.Status, last.At
				a.LastStatus, a.LastUsedAt = &status, &at
			}
		}
		a.TokenExpiresAt = buffTokenExpiry(auth)
		if reserve := coreauth.ReserveOf(auth); !reserve.IsZero() {
			a.Reserve = &reserve
		}
		a.Held = []buffReserveHold{}
		for _, hold := range coreauth.ReserveHolds(auth, "", now) {
			a.Held = append(a.Held, buffReserveHold{Window: hold.Window, Used: hold.Used, Reserve: hold.Reserve, Until: epochMs(hold.Until)})
		}
		if ws, ok := authWebsocketsValue(auth); ok {
			a.Websockets = &ws
		}
		if strings.EqualFold(auth.Provider, "codex") {
			a.Codex = &buffCodex{Signals: map[string]string{}}
			for k, v := range auth.Quota.Signals {
				a.Codex.Signals[k] = v
			}
			if withCodexUsage {
				usage := buffCodexProbe.Get(ctx, h.buffHTTPClient(auth), auth.ID, func() (buffcloud.CodexCredential, error) {
					return buffCodexCredential(auth)
				}, now)
				a.Codex.Usage = &usage
			}
		}
		accounts = append(accounts, a)
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts
}

// buffLinkedMetadata is a copy of the auth's metadata with a linked credential's
// current token read from its dir, as the executor does before each request.
func buffLinkedMetadata(auth *coreauth.Auth) (map[string]any, error) {
	metadata := make(map[string]any, len(auth.Metadata))
	for k, v := range auth.Metadata {
		metadata[k] = v
	}
	if linked.Dir(metadata) == "" {
		return metadata, nil
	}
	return metadata, linked.Sync(metadata)
}

func buffTokenExpiry(auth *coreauth.Auth) *int64 {
	metadata, err := buffLinkedMetadata(auth)
	if err != nil {
		return nil
	}
	view := &coreauth.Auth{Metadata: metadata, Attributes: auth.Attributes}
	if exp, ok := view.AccessTokenExpirationTime(); ok {
		return epochMs(exp)
	}
	return nil
}

func buffCodexCredential(auth *coreauth.Auth) (buffcloud.CodexCredential, error) {
	metadata, err := buffLinkedMetadata(auth)
	if err != nil {
		return buffcloud.CodexCredential{}, err
	}
	token, _ := metadata["access_token"].(string)
	account, _ := metadata["account_id"].(string)
	return buffcloud.CodexCredential{AccessToken: token, AccountID: account}, nil
}

// buffHTTPClient honours the credential's proxy-url, then the global one. The
// timeout bounds a management read, not a proxied request.
func (h *Handler) buffHTTPClient(auth *coreauth.Auth) *http.Client {
	client := &http.Client{Timeout: 10 * time.Second}
	proxyURL := strings.TrimSpace(auth.ProxyURL)
	if proxyURL == "" {
		h.mu.Lock()
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
		h.mu.Unlock()
	}
	if proxyURL != "" {
		util.SetProxy(&config.SDKConfig{ProxyURL: proxyURL}, client)
	}
	return client
}

func epochMs(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

// BuffMessages serves GET /v8/management/buff/messages: the messages log,
// newest first. Filters: before and since (epoch ms), traceId, session, account,
// q (text) and limit (at most 200). more says whether older rows match too.
func (h *Handler) BuffMessages(c *gin.Context) {
	l := buffcloud.DefaultMessagesLog()
	if l == nil {
		c.JSON(http.StatusOK, gin.H{"messages": []buffcloud.MessageRow{}, "more": false})
		return
	}
	q := buffcloud.MessageQuery{
		TraceID: strings.TrimSpace(c.Query("traceId")),
		Session: strings.TrimSpace(c.Query("session")),
		Account: strings.TrimSpace(c.Query("account")),
		Text:    strings.TrimSpace(c.Query("q")),
	}
	for name, dst := range map[string]*int64{"before": &q.Before, "since": &q.Since} {
		if raw := strings.TrimSpace(c.Query(name)); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": name + " must be epoch milliseconds"})
				return
			}
			*dst = parsed
		}
	}
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive number"})
			return
		}
		q.Limit = n
	}
	rows, more, err := l.Query(q)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if rows == nil {
		rows = []buffcloud.MessageRow{}
	}
	c.JSON(http.StatusOK, gin.H{"messages": rows, "more": more})
}
