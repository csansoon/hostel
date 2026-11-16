package helper

// lockMachine: on Windows, begin and cleanup keep no persistence copy.
func (h *helper) lockMachine() (func(), error) { return func() {}, nil }
