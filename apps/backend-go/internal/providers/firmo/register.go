package firmo

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"

func init() {
	providers.Register(ipinfo{})
	providers.Register(gleif{})
	providers.Register(companiesHouse{})
	providers.Register(mcaRegistry{})
	providers.Register(wikidata{})
	providers.Register(secEdgar{})
	providers.Register(jsonldFirmo{})
	providers.Register(techStack{})
	providers.Register(atsHiring{})
	providers.Register(googleMaps{})
}
