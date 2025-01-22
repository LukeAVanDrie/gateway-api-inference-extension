package scheduling

// Request is a structured representation of the fields we parse out of the
// request body.
type Request struct {
	// Indicates whether the request is critical.
	IsCritical bool
	// The request's model.
	Model string
	// The request's final resolved target model after traffic splitting.
	ResolvedTargetModel string
}
