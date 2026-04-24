package agentsdk

import "context"

// Run and lazyRun are carried through context so public Agent methods can
// be called from anywhere — handler bodies, helper functions, goroutines —
// without threading a *Run through every signature. The types themselves
// stay unexported; builders never touch them.

type runCtxKey struct{}
type lazyRunCtxKey struct{}

func contextWithRun(ctx context.Context, r *run) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, runCtxKey{}, r)
}

func runFromContext(ctx context.Context) *run {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(runCtxKey{}).(*run)
	return r
}

func contextWithLazyRun(ctx context.Context, l *lazyRun) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, lazyRunCtxKey{}, l)
}

func lazyRunFromContext(ctx context.Context) *lazyRun {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(lazyRunCtxKey{}).(*lazyRun)
	return l
}
