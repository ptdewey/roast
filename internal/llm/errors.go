package llm

import "errors"

// ErrNoFinalCall means finalization exhausted its bounded submission attempts
// without a valid final tool call. The finder reports incomplete coverage;
// verifier failures leave candidates unverified rather than refuting them.
var ErrNoFinalCall = errors.New("llm: tool loop ended without a final tool call")

// ErrWorkDeadline marks a Converse loop cut off by ConverseOpts.WorkDeadline.
// Always wrapped together with ErrNoFinalCall (the deadline fired AND the
// wrap-up turn produced no submission) so errors.Is matches both the generic
// no-submission policy and the specific stop reason.
var ErrWorkDeadline = errors.New("llm: work deadline reached")

// ErrAuth wraps 401/403 from an inference endpoint.
var ErrAuth = errors.New("llm: authentication failed")

// ErrUsageLimit marks a provider-declared OpenAI subscription usage limit, not an
// authentication failure or the local token reservation budget.
var ErrUsageLimit = errors.New("llm: OpenAI subscription usage limit reached")

// ErrEndpoint marks a non-auth inference HTTP or transport failure.
var ErrEndpoint = errors.New("llm: inference endpoint failure")
