package providers

// Cleaner is the part of *testing.T RegisterForTest needs (declared here so production code never imports "testing").
type Cleaner interface{ Cleanup(func()) }

// RegisterForTest registers p for the duration of one test and removes it again in Cleanup, so a test that registers a
// fixed provider name survives -count=N and shuffled runs (Register panics on a duplicate).
func RegisterForTest(t Cleaner, p Provider) {
	Register(p)
	name := p.Name()
	t.Cleanup(func() {
		regMu.Lock()
		defer regMu.Unlock()
		delete(reg, name)
	})
}
