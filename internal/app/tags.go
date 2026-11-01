package app

import (
	"context"
	"sort"

	gitrepo "elephant/internal/git"
	"elephant/internal/model"
	"elephant/internal/storage"
)

// TagCount is one tag used in a project and how many entries carry it.
type TagCount struct {
	Tag     string `json:"tag"`
	Entries int    `json:"entries"`
}

// Tags lists the project's tags with entry counts, most used first, then by
// name. It reads every entry's relations, so its cost grows with the project.
func (s *Service) Tags(ctx context.Context, cwd string) (out []TagCount, err error) {
	err = s.within(ctx, cwd, func(tx storage.Tx, p model.Project, g gitrepo.Metadata) error {
		counts := map[string]int{}
		for offset := 0; ; offset += 200 {
			page, err := tx.List(p, model.Filter{Limit: 200, Offset: offset})
			if err != nil {
				return err
			}
			for _, e := range page {
				links, err := tx.Relations(model.EntryNode(p, e.ID))
				if err != nil {
					return err
				}
				for _, tag := range tagsOf(links) {
					counts[tag]++
				}
			}
			if len(page) < 200 {
				break
			}
		}
		out = make([]TagCount, 0, len(counts))
		for tag, n := range counts {
			out = append(out, TagCount{Tag: tag, Entries: n})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Entries != out[j].Entries {
				return out[i].Entries > out[j].Entries
			}
			return out[i].Tag < out[j].Tag
		})
		return nil
	})
	return
}
