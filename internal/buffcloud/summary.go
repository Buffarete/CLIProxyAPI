package buffcloud

import (
	"math"
	"time"
)

// Room is one rate-limit window of a credential as last observed.
type Room struct {
	// Window is "5h" or "7d" for Claude, "primary" or "secondary" for Codex.
	Window string `json:"window"`
	// Used is the fraction of the window spent, 0 to 1.
	Used float64 `json:"used"`
	// ResetsAt is epoch ms, nil when unknown.
	ResetsAt *int64 `json:"resetsAt"`
}

// Account is what the summary needs to know about one credential.
type Account struct {
	ID            string  `json:"id"`
	Provider      string  `json:"provider"`
	Label         string  `json:"label"`
	Plan          *string `json:"plan"`
	Enabled       bool    `json:"enabled"`
	Rooms         []Room  `json:"rooms"`
	CooldownUntil *int64  `json:"cooldownUntil"`
	LastStatus    *int    `json:"lastStatus"`
}

// Count is requests and tokens over a period. Tokens is Split's total.
type Count struct {
	Requests int    `json:"requests"`
	Tokens   int64  `json:"tokens"`
	Split    Tokens `json:"split"`
}

func (c *Count) add(row Row) {
	c.Requests++
	if row.Tokens != nil {
		c.Split = c.Split.add(*row.Tokens)
		c.Tokens = c.Split.Total()
	}
}

// RoomView is a room as the AI page draws it: Codex's primary and secondary
// windows are the same 5 hours and 7 days Claude reports.
type RoomView struct {
	Window string  `json:"window"`
	Left   float64 `json:"left"`
	// ResetsAt is epoch ms, nil when unknown or already passed.
	ResetsAt *int64 `json:"resetsAt"`
}

// SummaryAccount is one credential's line on the AI page.
type SummaryAccount struct {
	ID       string  `json:"id"`
	Provider string  `json:"provider"`
	Label    string  `json:"label"`
	Plan     *string `json:"plan"`
	Enabled  bool    `json:"enabled"`
	// Rooms is empty until the proxy has had a response from this account.
	Rooms         []RoomView `json:"rooms"`
	CooldownUntil *int64     `json:"cooldownUntil"`
	LastStatus    *int       `json:"lastStatus"`
	LastHour      Count      `json:"lastHour"`
	Today         Count      `json:"today"`
}

// LastRequest is the most recent request of all accounts.
type LastRequest struct {
	AccountID string `json:"accountId"`
	At        int64  `json:"at"`
	Status    int    `json:"status"`
}

// Summary is what buff-cloud's ai-proxy.ts summariseProxy computed, plus each
// count's token split and today's counts by model.
type Summary struct {
	Now          int64             `json:"now"`
	DayStart     int64             `json:"dayStart"`
	Accounts     []SummaryAccount  `json:"accounts"`
	LastHour     Count             `json:"lastHour"`
	Today        Count             `json:"today"`
	TodayByModel map[string]*Count `json:"todayByModel"`
	Last         *LastRequest      `json:"last"`
}

// UnknownModel keys todayByModel for rows without a model.
const UnknownModel = "unknown"

// Summarise counts rows into the last hour (started at or after now - 1 h) and
// today (started at or after dayStart), per account and in total.
func Summarise(accounts []Account, rows []Row, now, dayStart time.Time) Summary {
	nowMs, dayMs, hourMs := now.UnixMilli(), dayStart.UnixMilli(), now.Add(-time.Hour).UnixMilli()
	s := Summary{Now: nowMs, DayStart: dayMs, Accounts: make([]SummaryAccount, 0, len(accounts)), TodayByModel: map[string]*Count{}}
	index := make(map[string]int, len(accounts))
	for i, a := range accounts {
		index[a.ID] = i
		view := SummaryAccount{
			ID: a.ID, Provider: a.Provider, Label: a.Label, Plan: a.Plan, Enabled: a.Enabled,
			Rooms: roomViews(a.Rooms, nowMs), LastStatus: a.LastStatus,
		}
		if a.CooldownUntil != nil && *a.CooldownUntil > nowMs {
			view.CooldownUntil = a.CooldownUntil
		}
		s.Accounts = append(s.Accounts, view)
	}
	for _, row := range rows {
		i, known := index[row.AccountID]
		if row.At >= hourMs {
			s.LastHour.add(row)
			if known {
				s.Accounts[i].LastHour.add(row)
			}
		}
		if row.At >= dayMs {
			s.Today.add(row)
			if known {
				s.Accounts[i].Today.add(row)
			}
			model := UnknownModel
			if row.Model != nil {
				model = *row.Model
			}
			if s.TodayByModel[model] == nil {
				s.TodayByModel[model] = &Count{}
			}
			s.TodayByModel[model].add(row)
		}
		if s.Last == nil || row.At > s.Last.At {
			s.Last = &LastRequest{AccountID: row.AccountID, At: row.At, Status: row.Status}
		}
	}
	return s
}

var viewWindow = map[string]string{"5h": "5h", "primary": "5h", "7d": "7d", "secondary": "7d"}

// roomViews uses the router's rule (sdk/cliproxy/auth RoomLeft): a window whose
// reset has passed counts as full.
func roomViews(rooms []Room, nowMs int64) []RoomView {
	views := make([]RoomView, 0, len(rooms))
	for _, r := range rooms {
		view := RoomView{Window: viewWindow[r.Window], Left: math.Max(0, 1-r.Used)}
		if r.ResetsAt != nil {
			if *r.ResetsAt <= nowMs {
				view.Left = 1
			} else {
				view.ResetsAt = r.ResetsAt
			}
		}
		if view.Window == "" {
			view.Window = r.Window
		}
		views = append(views, view)
	}
	return views
}
