package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/remote"
)

type remoteScope struct {
	scope
	Remote string `json:"remote" jsonschema:"Registered peer name; remote_recall also accepts a source Elephant UUID"`
}
type remoteCreate struct {
	remoteScope
	app.CreateInput
}

func registerRemotes(s *sdk.Server, service *app.Service, cwd func(string) string) {
	register(s, "list_remotes", "List configured remote peers without contacting them.", func(ctx context.Context, in struct{}) ([]model.Remote, error) { return service.ListRemotes(ctx) })
	register(s, "remote_recall", "Show locally received state from one source Elephant, kept separate from local state.", func(ctx context.Context, in remoteScope) (app.RecallResult, error) {
		return service.RecallRemote(ctx, cwd(in.CWD), in.Remote)
	})
	register(s, "ensure_remote_project", "Ask a peer to create this Elephant's source table without changing the peer's local state.", func(ctx context.Context, in remoteScope) (app.Response, error) {
		m, err := service.BuildMessage(ctx, cwd(in.CWD), "project.ensure", "", app.CreateInput{})
		if err != nil {
			return app.Response{}, err
		}
		return service.Send(ctx, remote.ASH{}, in.Remote, m)
	})
	for _, kind := range []model.Kind{model.Fact, model.Decision, model.Task} {
		register(s, "remote_send_"+string(kind), "Send a new "+string(kind)+" into this Elephant's source table on a registered peer, preserving actor provenance and tags. adopt_entry copies the tags.", func(ctx context.Context, in remoteCreate) (app.Response, error) {
			m, err := service.BuildMessage(ctx, cwd(in.CWD), "entry.send", kind, in.CreateInput)
			if err != nil {
				return app.Response{}, err
			}
			return service.Send(ctx, remote.ASH{}, in.Remote, m)
		})
	}
}
