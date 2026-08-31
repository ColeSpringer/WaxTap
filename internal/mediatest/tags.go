package mediatest

import (
	"context"
	"fmt"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

// TagFile writes alternating key/value pairs onto path through WaxLabel, the
// library that owns metadata on finished files, so fixture tagging cannot
// drift from the production write path. An odd pair count is an error.
func TagFile(ctx context.Context, path string, pairs ...string) error {
	if len(pairs)%2 != 0 {
		return fmt.Errorf("mediatest.TagFile: %d strings do not form key/value pairs", len(pairs))
	}
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		return err
	}
	ed := doc.Edit()
	for i := 0; i < len(pairs); i += 2 {
		key, err := tag.ParseKey(pairs[i])
		if err != nil {
			return fmt.Errorf("key %q: %w", pairs[i], err)
		}
		ed.Set(key, pairs[i+1])
	}
	plan, err := ed.Prepare()
	if err != nil {
		return err
	}
	_, _, err = plan.Execute(ctx, waxlabel.SaveBack())
	return err
}
