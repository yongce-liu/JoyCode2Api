package joycode

// ResolveModel picks the upstream model id for a request.
//
// A client-supplied id is preserved verbatim. The live tenant catalog and the
// upstream registry are the only authorities on which ids exist, so a proxy
// that "helpfully" rewrites an unrecognised id to a default answers with a
// different model than the caller asked for — and the caller has no way to
// tell. An id that genuinely does not exist is rejected upstream (6002), which
// names the offending model instead of hiding it.
//
// An empty id has nothing to preserve, so it falls back to the account
// default, then the system default, then DefaultModel.
//
// Both protocol front-ends (OpenAI and Anthropic) resolve through here so the
// two cannot drift apart.
func ResolveModel(model string, accountDefault string, systemDefault string) string {
	if model != "" {
		return model
	}
	if accountDefault != "" {
		return accountDefault
	}
	if systemDefault != "" {
		return systemDefault
	}
	return DefaultModel
}
