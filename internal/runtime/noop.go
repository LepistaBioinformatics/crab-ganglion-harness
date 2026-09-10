package runtime

import (
	"context"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Explicit no-op ports, installed by withDefaults when a field is left unset.
//
// These exist instead of `if l.Approver == nil` checks at each call site. The
// nil check is not merely uglier -- it is WRONG for a typed nil, which is a
// non-nil interface holding a nil pointer, and the composition root produces
// exactly that shape when a constructor returns (*T)(nil). A no-op value cannot
// be got wrong that way.

// allowAll is the Approver used when none is configured: the v1 empty gate list.
// Every action is asked about and allowed immediately, so the approval path is
// exercised from the first deploy rather than sitting untested until it matters.
type allowAll struct{}

func (allowAll) Request(context.Context, domain.ActionRequest) (domain.Decision, error) {
	return domain.Decision{Allowed: true}, nil
}

// noTelemetry records nothing.
type noTelemetry struct{}

func (noTelemetry) Span(ctx context.Context, _ string, _ ...domain.Attr) (context.Context, func(error)) {
	return ctx, func(error) {}
}

func (noTelemetry) Usage(context.Context, domain.Usage, ...domain.Attr) {}
