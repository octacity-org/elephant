package app_test

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/remote"
	"elephant/internal/remote/ash"
	"elephant/internal/storage/zova"
)

// ashPeer stands in for `ash execv HOST --stdin -- elephant receive`: it
// hands the forwarded stdin to another Elephant and returns its response.
type ashPeer struct {
	target *app.Service
	args   []string
}

func (p *ashPeer) Run(ctx context.Context, _ string, args []string, stdin io.Reader, stdout, _ io.Writer) error {
	p.args = args
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	var m app.Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return err
	}
	response, err := p.target.Receive(ctx, m)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(response)
}

// Any entry that is valid locally can be sent: a fact with the largest body,
// full of characters JSON may escape, fits the ASH transport.
func TestSendLargestValidEntryOverASH(t *testing.T) {
	ctx := context.Background()
	cwd := sharedRepo(t)
	open := func(name string) *app.Service {
		db, err := zova.Open(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return app.New(db)
	}
	sender, receiver := open("a.zova"), open("b.zova")
	if _, err := sender.AddRemote(ctx, "peer", "ash", `{"host":"peer"}`); err != nil {
		t.Fatal(err)
	}
	line := "if a<b&&c>d {}\n"
	body := strings.Repeat(line, 32768/len(line))
	body += strings.Repeat("&", 32768-len(body))
	m, err := sender.BuildMessage(ctx, cwd, "entry.send", model.Fact, app.CreateInput{Title: strings.Repeat("<", 300), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	peer := &ashPeer{target: receiver}
	backend := remote.ASH{Backend: ash.Backend{Executor: peer}}
	if _, err := sender.Send(ctx, backend, "peer", m); err != nil {
		t.Fatalf("send a %d-byte body: %v", len(body), err)
	}
	if want := []string{"execv", "peer", "--stdin", "--", "elephant", "receive"}; !reflect.DeepEqual(peer.args, want) {
		t.Fatalf("ash arguments = %q, want %q", peer.args, want)
	}
	self, _ := sender.Identity(ctx)
	got, err := receiver.RecallRemote(ctx, cwd, self)
	if err != nil || len(got.Facts) != 1 || got.Facts[0].Body != body {
		t.Fatalf("receiver did not store the body unchanged: %v", err)
	}

	// A request over the input limit fails before ASH runs, and says why.
	peer.args = nil
	_, err = backend.Exchange(ctx, model.Remote{Backend: "ash", BackendConfig: `{"host":"peer"}`}, []byte(`"`+strings.Repeat("x", ash.MaxInputSize)+`"`))
	if err == nil || !strings.Contains(err.Error(), "65536") || peer.args != nil {
		t.Fatalf("oversized request = %v (ran: %v), want a limit error before running ASH", err, peer.args != nil)
	}
}
