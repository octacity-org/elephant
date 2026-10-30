package receive

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"uuid"

	"elephant/internal/app"
	"elephant/internal/storage/zova"
)

func TestMachineBoundary(t *testing.T) {
	db, err := zova.Open(filepath.Join(t.TempDir(), "receive.zova"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := app.New(db)
	id := func() string { return uuid.NewV7().String() }
	m := app.Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: id(), Project: app.ProjectRef{Identity: "github.com/test/repo", Name: "repo"}, Operation: "project.ensure"}
	data, _ := json.Marshal(m)
	for _, tc := range []struct {
		name, payload string
		bad           bool
	}{{"valid", string(data), false}, {"repeat", string(data), false}, {"malformed", "{", true}, {"trailing", string(data) + "{}", true}, {"oversize", strings.Repeat(" ", MaxMessageBytes+1), true}, {"unknown field", strings.TrimSuffix(string(data), "}") + `,"unexpected":true}`, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := Handle(context.Background(), s, strings.NewReader(tc.payload), &out); err != nil {
				t.Fatal(err)
			}
			var r app.Response
			if err := json.Unmarshal(out.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
			if (r.Error != "") != tc.bad {
				t.Fatal(out.String())
			}
		})
	}
}
