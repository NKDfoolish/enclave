package domain

import "time"

// AttemptStatus is how one provider call ended.
type AttemptStatus string

const (
	// AttemptOK: the provider answered in full.
	AttemptOK AttemptStatus = "ok"
	// AttemptFailed: the call failed before any output; the gateway may have fallen back.
	AttemptFailed AttemptStatus = "failed"
	// AttemptInterrupted: the call failed after output had started reaching the caller.
	AttemptInterrupted AttemptStatus = "interrupted"
	// AttemptCancelled: the caller went away; the gateway stopped the provider call.
	AttemptCancelled AttemptStatus = "cancelled"
)

// UsageRecord accounts for one provider call. A request that falls back produces one record
// per route tried, so the cost of failed attempts is visible too.
type UsageRecord struct {
	Call      Call
	Kind      ModelKind
	Alias     string
	Route     Route
	Attempt   int
	Status    AttemptStatus
	ErrorCode string
	Usage     Usage
	// Estimated is true when the provider never reported usage (for example because the
	// stream was cut short) and the gateway estimated it from the text it saw.
	Estimated    bool
	CostMicroUSD int64
	Latency      time.Duration
	// FirstToken is the time until the first output arrived; nil when none did.
	FirstToken *time.Duration
}

// EstimateTokens is a rough token count for text whose real count the provider never
// reported: about four characters per token for English, fewer for Vietnamese. Records that
// use it are flagged Estimated.
func EstimateTokens(characters int) int {
	return (characters + 3) / 4
}
