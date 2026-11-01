package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"elephant/internal/app"
)

type diffInput struct {
	scope
	Remote string `json:"remote" jsonschema:"Registered peer name or source Elephant UUID"`
	Global bool   `json:"global,omitempty" jsonschema:"Compare what the peer sent from its global memory with local global memory"`
}

type adoptInput struct {
	scope
	ID     string `json:"id" jsonschema:"Remote entry ID to adopt"`
	Remote string `json:"remote" jsonschema:"Registered peer name or source Elephant UUID"`
	Global bool   `json:"global,omitempty" jsonschema:"Adopt an entry the peer sent from its global memory into local global memory"`
}

func registerAdopt(s *sdk.Server, service *app.Service, cwd func(string) string) {
	register(s, "remote_inbox", "List received additions grouped by source Elephant and project, never merging sources.", func(ctx context.Context, in scope) ([]app.InboxGroup, error) {
		return service.Inbox(ctx)
	})
	register(s, "remote_diff", "Compare one remote source table with local state while preserving both boundaries. global compares what the peer sent from its global memory with local global memory.", func(ctx context.Context, in diffInput) (app.DiffResult, error) {
		if in.Global {
			return service.DiffGlobal(ctx, cwd(in.CWD), in.Remote)
		}
		return service.Diff(ctx, cwd(in.CWD), in.Remote)
	})
	register(s, "adopt_entry", "Explicitly promote one received entry into local truth with provenance; idempotent on repeat. global adopts an entry sent from the peer's global memory into local global memory, with its tags.", func(ctx context.Context, in adoptInput) (app.AdoptResult, error) {
		if in.Global {
			return service.AdoptGlobal(ctx, cwd(in.CWD), in.ID, in.Remote)
		}
		return service.Adopt(ctx, cwd(in.CWD), in.ID, in.Remote)
	})
}
