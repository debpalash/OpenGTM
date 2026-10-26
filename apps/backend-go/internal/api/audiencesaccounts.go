package api

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/httpx"

// audAccountsMaxMembers overrides AUDIENCE_ACCOUNTS_MAX_MEMBERS when non-zero (tests shrink it).
var audAccountsMaxMembers int

// registerAudiencesAccounts registers its routes (plan 10); stub until its task lands.
func registerAudiencesAccounts(r *httpx.Router, d Deps) {}
