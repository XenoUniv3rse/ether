package artwork

import (
	"context"
	"crypto/sha256"
	"fmt"

	kitimages "github.com/braedonsaunders/ether/internal/frontend/kit/images"
)

// Variant stores a transform of already-authorized bytes in the shared memory
// and disk cache. The digest keeps changed source bytes out of older variants.
func (im *Images) Variant(ctx context.Context, data []byte, contentType, transform string, render func(context.Context) ([]byte, string, error)) ([]byte, string, error) {
	if kitimages.NoStore(ctx) {
		return render(ctx)
	}
	key := fmt.Sprintf("image-variant-v1/%x/%s/%s", sha256.Sum256(data), contentType, transform)
	return im.get(ctx, key, render)
}
