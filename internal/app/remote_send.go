package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"
	"uuid"

	"elephant/internal/model"
	"elephant/internal/remote"
	"elephant/internal/storage"
)

func (s *Service) AddRemote(ctx context.Context, name, backend, config string) (out model.Remote, err error) {
	if !validText(name, 100) || validID(name) {
		return out, fmt.Errorf("%w: remote name required (not a UUID)", model.ErrInvalidInput)
	}
	if err = remote.Validate(backend, config); err != nil {
		return out, err
	}
	now := time.Now().UTC()
	out = model.Remote{ID: uuid.NewV7().String(), Name: name, Backend: backend, BackendConfig: config, CreatedAt: now, UpdatedAt: now}
	err = s.store.Transact(ctx, func(tx storage.Tx) error {
		rs, err := tx.Remotes()
		if err != nil {
			return err
		}
		for _, r := range rs {
			if r.Name == name {
				return fmt.Errorf("%w: duplicate remote name", model.ErrInvalidInput)
			}
		}
		return tx.SaveRemote(out)
	})
	return
}
func (s *Service) Send(ctx context.Context, b remote.Backend, name string, m Message) (out Response, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", model.ErrRemote, err)
		}
	}()
	if err = validateMessage(m); err != nil {
		return out, err
	}
	self, err := s.Identity(ctx)
	if err != nil {
		return out, err
	}
	if m.SenderElephantID != self {
		return out, fmt.Errorf("%w: message must originate from this Elephant", model.ErrInvalidInput)
	}
	r, err := s.Remote(ctx, name)
	if err != nil {
		return out, err
	}
	// HTML escaping would turn each <, > and & into six bytes; without it a
	// body of code still fits the transport's input limit.
	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(m); err != nil {
		return out, err
	}
	data, err := b.Exchange(ctx, r, payload.Bytes())
	if err != nil {
		return out, err
	}
	if err = json.Unmarshal(data, &out); err != nil {
		return out, fmt.Errorf("invalid remote response: %w", err)
	}
	if out.ProtocolVersion != 1 || out.MessageID != m.MessageID || !validID(out.ElephantID) {
		return out, fmt.Errorf("invalid remote response identity or version")
	}
	if out.Error != "" {
		return out, fmt.Errorf("remote: %s", out.Error)
	}
	if out.ElephantID == self || out.Table.Scope != "remote" || out.Table.SourceElephantID != self || out.Table.Identity != m.Project.Identity {
		return out, fmt.Errorf("invalid remote table ownership")
	}
	if r.ElephantID != "" && r.ElephantID != out.ElephantID {
		return out, fmt.Errorf("remote Elephant identity changed; remove and register the peer again")
	}
	r.ElephantID = out.ElephantID
	r.UpdatedAt = time.Now().UTC()
	err = s.store.Transact(ctx, func(tx storage.Tx) error { return tx.SaveRemote(r) })
	return
}
