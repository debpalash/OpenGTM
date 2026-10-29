package config

import (
	"reflect"
	"testing"
	"time"
)

func TestAudienceConfigDefaultsMatchPython(t *testing.T) {
	a, err := loadAudiences(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	want := AudienceSettings{
		DestTokenTTLDays: 90, RefreshPageSize: 500, DestMemberPageSize: 500, DestAdBatchSize: 5000,
		AccountsMaxMembers: 200000, DestDeliveryTimeout: 30 * time.Second,
	}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("defaults\n got %#v\nwant %#v", a, want)
	}
}

func TestAudienceSettingsParse(t *testing.T) {
	env := map[string]string{
		"GO_AUDIENCE_ROUTES_ENABLED": "true", "GO_DEST_ROUTES_ENABLED": "1", "GO_EXECUTOR_AUDIENCE_REFRESH": "yes",
		"GO_DEST_TYPES": " Webhook, slack ,,WEBHOOK", "OPENGTM_DESTINATION_TOKEN_TTL_DAYS": "30",
		"AUDIENCE_REFRESH_PAGE_SIZE": "10", "DEST_MEMBER_PAGE_SIZE": "20", "DEST_AD_BATCH_SIZE": "30",
		"AUDIENCE_ACCOUNTS_MAX_MEMBERS": "40", "DEST_DELIVERY_TIMEOUT": "5",
	}
	a, err := loadAudiences(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := AudienceSettings{GoAudienceRoutesEnabled: true, GoDestRoutesEnabled: true, GoExecutorAudienceRefresh: true,
		GoDestTypes: []string{"webhook", "slack"}, DestTokenTTLDays: 30, RefreshPageSize: 10, DestMemberPageSize: 20,
		DestAdBatchSize: 30, AccountsMaxMembers: 40, DestDeliveryTimeout: 5 * time.Second}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("parsed\n got %#v\nwant %#v", a, want)
	}
}

// The token TTL is clamped to 1-365 like Python's inbound.py (not rejected).
func TestDestinationTokenTTLClamped(t *testing.T) {
	for in, want := range map[string]int{"0": 1, "-5": 1, "9999": 365, "365": 365, "7": 7} {
		a, err := loadAudiences(func(k string) string {
			if k == "OPENGTM_DESTINATION_TOKEN_TTL_DAYS" {
				return in
			}
			return ""
		})
		if err != nil || a.DestTokenTTLDays != want {
			t.Errorf("ttl %q: got %d err %v, want %d", in, a.DestTokenTTLDays, err, want)
		}
	}
}

func TestAudienceSettingsRejectBad(t *testing.T) {
	for _, kv := range [][2]string{
		{"GO_AUDIENCE_ROUTES_ENABLED", "maybe"}, {"GO_DEST_ROUTES_ENABLED", "x"}, {"GO_EXECUTOR_AUDIENCE_REFRESH", "2"},
		{"AUDIENCE_REFRESH_PAGE_SIZE", "0"}, {"DEST_MEMBER_PAGE_SIZE", "x"}, {"DEST_AD_BATCH_SIZE", "-1"},
		{"AUDIENCE_ACCOUNTS_MAX_MEMBERS", "0"}, {"DEST_DELIVERY_TIMEOUT", "0"}, {"OPENGTM_DESTINATION_TOKEN_TTL_DAYS", "x"},
	} {
		if _, err := loadAudiences(func(k string) string {
			if k == kv[0] {
				return kv[1]
			}
			return ""
		}); err == nil {
			t.Errorf("%s=%s accepted", kv[0], kv[1])
		}
	}
}
