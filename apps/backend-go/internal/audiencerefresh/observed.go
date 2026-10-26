package audiencerefresh

import "encoding/json"

// Audience is Audience.to_api (key order kept).
type Audience struct {
	ID                         string          `json:"id"`
	Name                       string          `json:"name"`
	Description                string          `json:"description"`
	Filters                    json.RawMessage `json:"filters"`
	MemberCount                int             `json:"member_count"`
	CreatedAt                  *string         `json:"created_at"`
	UpdatedAt                  *string         `json:"updated_at"`
	RefreshedAt                *string         `json:"refreshed_at"`
	RefreshEnabled             bool            `json:"refresh_enabled"`
	RefreshIntervalMinutes     int             `json:"refresh_interval_minutes"`
	NextRefreshAt              *string         `json:"next_refresh_at"`
	RefreshHealth              string          `json:"refresh_health"`
	LastRefreshError           *string         `json:"last_refresh_error"`
	ConsecutiveRefreshFailures int             `json:"consecutive_refresh_failures"`
}
