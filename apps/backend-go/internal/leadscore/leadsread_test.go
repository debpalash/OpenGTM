package leadscore

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/leadsread"

// leadsread.Lead is scoreable directly.
var _ Lead = leadsread.Lead{}
