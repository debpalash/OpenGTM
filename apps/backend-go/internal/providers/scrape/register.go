package scrape

import "github.com/debpalash/OpenGTM/apps/backend-go/internal/providers"

func init() {
	providers.Register(websiteScraper{})
	providers.Register(deepScraper{})
	providers.Register(companyIntel{})
	providers.Register(localBusiness{})
	providers.Register(CrossLinked{})
	providers.Register(emailHarvester{})
	providers.Register(facebookPages{})
	providers.Register(socialFinder{})
	providers.Register(ddgCompany{})
	providers.Register(ddgEmail{})
	providers.Register(decisionMaker{})
	providers.Register(JobSpy{})
	providers.Register(holehe{})
	providers.Register(staffSpy{})
	providers.Register(mailScout{})
	providers.Register(companySizeHeuristic{})
	providers.Register(leadScorer{})
}
