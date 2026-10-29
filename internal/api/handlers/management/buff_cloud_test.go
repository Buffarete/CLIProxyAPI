package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestValidateBuffPin(t *testing.T) {
	auths := []*coreauth.Auth{
		{ID: "linked-claude_6.json", Provider: "claude"},
		{ID: "linked-claude_2.json", Provider: "claude", Disabled: true, Status: coreauth.StatusDisabled},
	}
	for _, c := range []struct {
		id, wantErr string
	}{
		{"", ""},
		{"linked-claude_6.json", ""},
		{"linked-claude_2.json", "is disabled"},
		{"linked-claude_9.json", "no credential"},
		// Ids are exact, as the router looks them up.
		{"Linked-Claude_6.json", "no credential"},
	} {
		err := validateBuffPin(c.id, auths)
		if c.wantErr == "" && err != nil {
			t.Errorf("pin %q: %v, want accepted", c.id, err)
		}
		if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("pin %q: error %v, want %q", c.id, err, c.wantErr)
		}
	}
}

type buffTestServer struct {
	t       *testing.T
	path    string
	router  *gin.Engine
	manager *coreauth.Manager
}

func newBuffTestServer(t *testing.T, auths ...*coreauth.Auth) *buffTestServer {
	t.Helper()
	return newBuffTestServerWithStore(t, nil, auths...)
}

func newBuffTestServerWithStore(t *testing.T, store coreauth.Store, auths ...*coreauth.Auth) *buffTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\nrouting:\n    strategy: \"most-room\"\n    session-affinity: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(store, nil, nil)
	for _, a := range auths {
		if _, err := manager.Register(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(cfg, path, manager)
	router := gin.New()
	v8 := router.Group("/v8/management", func(c *gin.Context) { c.Set(ConfigV8ContextKey, true) })
	v8.GET("/buff/accounts", h.BuffAccounts)
	v8.PATCH("/buff/settings", h.PatchBuffSettings)
	v8.PATCH("/buff/reserve", h.PatchBuffReserve)
	v8.POST("/buff/recheck", h.PostBuffRecheck)
	v8.POST("/credentials/refresh", h.RefreshAuthFiles)
	return &buffTestServer{t: t, path: path, router: router, manager: manager}
}

func (s *buffTestServer) do(method, url, body string, wantStatus int) map[string]any {
	s.t.Helper()
	recorder := httptest.NewRecorder()
	s.router.ServeHTTP(recorder, httptest.NewRequest(method, url, strings.NewReader(body)))
	if recorder.Code != wantStatus {
		s.t.Fatalf("%s %s %s: status %d, want %d: %s", method, url, body, recorder.Code, wantStatus, recorder.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &out)
	return out
}

func (s *buffTestServer) config() *config.Config {
	s.t.Helper()
	cfg, err := config.LoadConfig(s.path)
	if err != nil {
		s.t.Fatal(err)
	}
	return cfg
}

func TestPatchBuffSettingsValidatesThenWritesConfig(t *testing.T) {
	dir := t.TempDir()
	fileAuth := func(id, provider, label string) *coreauth.Auth {
		path := filepath.Join(dir, id)
		if err := os.WriteFile(path, []byte(`{"type":"`+provider+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return &coreauth.Auth{ID: id, FileName: id, Provider: provider, Label: label, Attributes: map[string]string{"path": path}}
	}
	s := newBuffTestServer(t, fileAuth("linked-codex.json", "codex", "Codex"), fileAuth("linked-claude_6.json", "claude", "Claude 6"))
	before, _ := os.ReadFile(s.path)

	// Any invalid field rejects the whole patch and writes nothing.
	for _, body := range []string{
		`{"strategy":"cheapest"}`,
		`{"strategy":"soonest-reset","pinned":"linked-claude_9.json"}`,
		`{"proxyEnabled":"no"}`,
		`{"paused":true}`,
		`[]`,
	} {
		s.do(http.MethodPatch, "/v8/management/buff/settings", body, http.StatusBadRequest)
	}
	if after, _ := os.ReadFile(s.path); string(after) != string(before) {
		t.Fatalf("a rejected patch changed config.yaml:\n%s", after)
	}

	got := s.do(http.MethodPatch, "/v8/management/buff/settings",
		`{"strategy":"Soonest-Reset","pinned":"linked-claude_6.json","pinnedFallback":false,"proxyEnabled":false}`, http.StatusOK)
	if got["strategy"] != "soonest-reset" || got["pinned"] != "linked-claude_6.json" || got["pinnedFallback"] != false || got["proxyEnabled"] != false {
		t.Fatalf("settings after patch = %v", got)
	}
	routing := s.config().Routing
	if routing.Strategy != "soonest-reset" || routing.Pinned != "linked-claude_6.json" || routing.PinnedFallback == nil || *routing.PinnedFallback || !routing.Paused {
		t.Fatalf("config.yaml routing = %+v", routing)
	}

	got = s.do(http.MethodPatch, "/v8/management/buff/settings", `{"pinned":null,"proxyEnabled":true,"strategy":"most-room"}`, http.StatusOK)
	if got["pinned"] != nil || got["proxyEnabled"] != true || got["strategy"] != "most-room" {
		t.Fatalf("settings after clearing = %v", got)
	}
	if routing := s.config().Routing; routing.Pinned != "" || routing.Paused || routing.Strategy != "most-room" || !routing.SessionAffinity {
		t.Fatalf("config.yaml routing after clearing = %+v", routing)
	}

	accounts := s.do(http.MethodGet, "/v8/management/buff/accounts", "", http.StatusOK)
	if settings := accounts["settings"].(map[string]any); settings["strategy"] != "most-room" || settings["sessionAffinity"] != true {
		t.Fatalf("accounts settings = %v", settings)
	}
	list := accounts["accounts"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["id"] != "linked-claude_6.json" || list[0].(map[string]any)["provider"] != "anthropic" {
		t.Fatalf("accounts = %v", list)
	}
}

func TestBuffAndRefreshResponsesHoldNoToken(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "antigravity-1.json")
	if err := os.WriteFile(file, []byte(`{"type":"antigravity"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newBuffTestServer(t, &coreauth.Auth{
		ID: "antigravity-1.json", FileName: "antigravity-1.json", Provider: "antigravity", Status: coreauth.StatusActive,
		Attributes: map[string]string{"path": file},
		Metadata:   map[string]any{"type": "antigravity", "refresh_token": "ref-1", "access_token": "old-1"},
	})
	// The executor's refresh puts refreshed-token and refresh-token in memory.
	s.manager.RegisterExecutor(&refreshRecordExecutor{provider: "antigravity"})

	for _, req := range []struct{ method, url string }{
		{http.MethodPost, "/v8/management/credentials/refresh?name=antigravity-1.json"},
		{http.MethodGet, "/v8/management/buff/accounts"},
	} {
		recorder := httptest.NewRecorder()
		s.router.ServeHTTP(recorder, httptest.NewRequest(req.method, req.url, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", req.method, req.url, recorder.Code, recorder.Body.String())
		}
		for _, secret := range []string{"ref-1", "old-1", "refreshed-token", "refresh-token"} {
			if strings.Contains(recorder.Body.String(), secret) {
				t.Errorf("%s %s response holds %q: %s", req.method, req.url, secret, recorder.Body.String())
			}
		}
	}
	if auth, _ := s.manager.GetByID("antigravity-1.json"); auth.Metadata["access_token"] != "refreshed-token" {
		t.Fatalf("refresh did not run: access_token = %v", auth.Metadata["access_token"])
	}
}

func TestPatchBuffReserveSetsListsAndClears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "linked-claude_4.json")
	if err := os.WriteFile(path, []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	claude := &coreauth.Auth{ID: "linked-claude_4.json", FileName: "linked-claude_4.json", Provider: "claude", Label: "Claude 4", Attributes: map[string]string{"path": path}}
	claude.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0.3"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
		"Anthropic-Ratelimit-Unified-7d-Utilization": {"0.82"},
		"Anthropic-Ratelimit-Unified-7d-Reset":       {strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10)},
	}, time.Now())
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	claude.Metadata = map[string]any{"type": "claude"}
	s := newBuffTestServerWithStore(t, store, claude)

	for _, body := range []string{
		`{"id":"linked-claude_4.json","reserve":{"all":1}}`,
		`{"id":"linked-claude_4.json","reserve":{"percent":20}}`,
		`{"id":"linked-claude_4.json"}`,
		`{"reserve":{"all":0.2}}`,
	} {
		s.do(http.MethodPatch, "/v8/management/buff/reserve", body, http.StatusBadRequest)
	}
	s.do(http.MethodPatch, "/v8/management/buff/reserve", `{"id":"linked-claude_9.json","reserve":{"all":0.2}}`, http.StatusNotFound)

	got := s.do(http.MethodPatch, "/v8/management/buff/reserve", `{"id":"linked-claude_4.json","reserve":{"all":0.2,"windows":{"5h":0.5}}}`, http.StatusOK)
	reserve, _ := got["reserve"].(map[string]any)
	if reserve["all"] != 0.2 || reserve["windows"].(map[string]any)["5h"] != 0.5 {
		t.Fatalf("reserve = %v", got["reserve"])
	}
	held, _ := got["held"].([]any)
	if len(held) != 1 || held[0].(map[string]any)["window"] != "7d" || held[0].(map[string]any)["until"] == nil {
		t.Fatalf("held = %v, want 7d at 82%% over its 20%% reserve", got["held"])
	}
	stored, _ := s.manager.GetByID("linked-claude_4.json")
	if coreauth.ReserveOf(stored).For("7d") != 0.2 {
		t.Fatalf("auth metadata = %v", stored.Metadata)
	}
	if onDisk, _ := os.ReadFile(path); !strings.Contains(string(onDisk), `"buff_reserve"`) {
		t.Fatalf("auth file = %s", onDisk)
	}

	got = s.do(http.MethodPatch, "/v8/management/buff/reserve", `{"id":"linked-claude_4.json","reserve":null}`, http.StatusOK)
	if got["reserve"] != nil || len(got["held"].([]any)) != 0 {
		t.Fatalf("after clearing: reserve = %v, held = %v", got["reserve"], got["held"])
	}
	accounts := s.do(http.MethodGet, "/v8/management/buff/accounts", "", http.StatusOK)
	if account := accounts["accounts"].([]any)[0].(map[string]any); account["reserve"] != nil {
		t.Fatalf("accounts after clearing = %v", account)
	}
}

func TestPostBuffRecheckClearsCooldownAndAnswersFreshAccounts(t *testing.T) {
	later := time.Now().Add(60 * time.Hour)
	dir := t.TempDir()
	path := func(id string) map[string]string {
		p := filepath.Join(dir, id)
		if err := os.WriteFile(p, []byte(`{"type":"claude"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return map[string]string{"path": p}
	}
	spent := &coreauth.Auth{ID: "claude-7", FileName: "claude-7", Attributes: path("claude-7"), Provider: "claude", Label: "Claude 7",
		Unavailable: true, NextRetryAfter: later, Status: coreauth.StatusError,
		Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: later, ObservedAt: time.Now(),
			Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "1", "Anthropic-Ratelimit-Unified-7d-Reset": "4102444800"}}}
	other := &coreauth.Auth{ID: "claude-3", FileName: "claude-3", Attributes: path("claude-3"), Provider: "claude", Label: "Claude 3"}
	s := newBuffTestServer(t, spent, other)

	s.do(http.MethodPost, "/v8/management/buff/recheck", `{"ids": ["nope"]}`, http.StatusNotFound)
	s.do(http.MethodPost, "/v8/management/buff/recheck", `{"ids": 3}`, http.StatusBadRequest)
	out := s.do(http.MethodPost, "/v8/management/buff/recheck", `{"ids": ["claude-7"]}`, http.StatusOK)
	if ids, _ := out["rechecked"].([]any); len(ids) != 1 || ids[0] != "claude-7" {
		t.Fatalf("rechecked = %v", out["rechecked"])
	}
	if at, _ := out["recheckedAt"].(float64); at <= 0 {
		t.Fatalf("recheckedAt = %v", out["recheckedAt"])
	}
	accounts, _ := out["accounts"].([]any)
	var found map[string]any
	for _, a := range accounts {
		if m := a.(map[string]any); m["id"] == "claude-7" {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("claude-7 missing from %v", accounts)
	}
	if rooms, _ := found["rooms"].([]any); len(rooms) != 0 {
		t.Fatalf("rooms after recheck = %v, want none (not heard from)", rooms)
	}
	if found["cooldownUntil"] != nil {
		t.Fatalf("cooldownUntil after recheck = %v", found["cooldownUntil"])
	}
	if all := s.do(http.MethodPost, "/v8/management/buff/recheck", "", http.StatusOK); len(all["rechecked"].([]any)) != 2 {
		t.Fatalf("an empty body rechecked %v, want every account", all["rechecked"])
	}
}
