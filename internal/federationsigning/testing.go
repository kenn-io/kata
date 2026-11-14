package federationsigning

// SkipStartupForTest advances only the startup quarantine for tests.
func (v *Verifier) SkipStartupForTest() { v.started = v.started.Add(-Quarantine) }
