package httpx

// An error the user can fix themselves names the page where they fix it. The
// web UI reads these headers and puts a button on the error toast that opens
// that page. Headers leave the {"detail": ...} body unchanged.
const (
	NextStepHeader      = "X-Next-Step"
	NextStepLabelHeader = "X-Next-Step-Label"
)

// NextStep returns the headers naming the page that fixes an error. Pass it to
// Detail: httpx.Detail(w, status, msg, httpx.StepEmail...).
func NextStep(label, to string) []Header {
	return []Header{{NextStepHeader, to}, {NextStepLabelHeader, label}}
}

// Pages that fix the setup errors the API returns.
var (
	StepEmail        = NextStep("Open email settings", "/settings?tab=email")
	StepIntegrations = NextStep("Open integrations", "/settings?tab=integrations")
)
