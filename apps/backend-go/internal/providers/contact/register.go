package contact

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"

func init() {
	for _, action := range []string{"company", "person", "workforce", "people", "search", "similar"} {
		providers.Register(companyEnrich{action: action})
	}
	providers.Register(&hunter{})
	providers.Register(apollo{})
	providers.Register(prospeo{})
	providers.Register(snovio{})
	providers.Register(pdl{})
	providers.Register(debounce{})
	providers.Register(abstractAPI{})
	providers.Register(numverify{})
	providers.Register(newReacher())
}
