package plugins

import "context"

type collectionPluginKey struct{}

// CollectionPluginID identifies the installed plugin for a backend write or undo.
// Only the host supplies this context value; plugin documents cannot set it.
func CollectionPluginID(ctx context.Context) string {
	id, _ := ctx.Value(collectionPluginKey{}).(string)
	return id
}

func collectionContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, collectionPluginKey{}, id)
}

// CollectionInputError is a safe, actionable validation failure supplied by the
// collection backend. It survives the hosted runtime as a client error.
type CollectionInputError struct{ Message string }

func (e *CollectionInputError) Error() string { return e.Message }
