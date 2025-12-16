package verifier

func (verifier *Verifier) assertLocked() {
	if verifier.mux.TryLock() {
		verifier.mux.Unlock()

		panic("INTERNAL ERROR: verifier should have been locked!")
	}
}
