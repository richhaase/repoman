// Package progress carries optional, synchronous presentation events. Events
// describe work; final engine results remain the reporting authority.
package progress

import "context"

type Event struct {
	Phase  string
	Path   string
	Detail string
	Value  any
}

type key struct{}

func WithReporter(ctx context.Context, report func(Event)) context.Context {
	return context.WithValue(ctx, key{}, report)
}

func Report(ctx context.Context, event Event) {
	if report, ok := ctx.Value(key{}).(func(Event)); ok {
		report(event)
	}
}

func HasReporter(ctx context.Context) bool { _, ok := ctx.Value(key{}).(func(Event)); return ok }
