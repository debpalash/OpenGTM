package contact

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"

func init() {
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
