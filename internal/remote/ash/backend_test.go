package ash

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type executorFunc func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error

func (f executorFunc) Run(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	return f(ctx, name, args, stdin, stdout, stderr)
}

func TestExchangeInvocationAndRetryPreservePayload(t *testing.T) {
	payload := []byte(`{"message_id":"same-id","title":"O'Reilly $(touch /tmp/nope); ` + "`id`" + `"}`)
	var inputs [][]byte
	b := Backend{Executor: executorFunc(func(ctx context.Context, name string, args []string, in io.Reader, out, errout io.Writer) error {
		want := []string{"--config", "/a config.toml", "execv", "fedora", "--stdin", "--", "elephant", "receive"}
		if name != "ash" || !reflect.DeepEqual(args, want) {
			t.Fatalf("unexpected invocation: %s %q", name, args)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing bounded deadline")
		}
		input, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		inputs = append(inputs, input)
		_, err = io.WriteString(out, `{"ok":true}`)
		return err
	})}
	for range 2 {
		got, err := b.Exchange(context.Background(), Config{Host: "fedora", ConfigPath: "/a config.toml"}, payload)
		if err != nil || string(got) != `{"ok":true}` {
			t.Fatalf("%s: %v", got, err)
		}
	}
	// The payload reaches the receiver's stdin byte for byte, and a retry
	// sends the same request.
	for _, input := range inputs {
		if !bytes.Equal(input, payload) {
			t.Fatalf("stdin = %q, want %q", input, payload)
		}
	}
}

func TestExchangeRejectsInvalidInputAndOutput(t *testing.T) {
	for _, tc := range []struct {
		name              string
		config            Config
		payload, response string
		runErr            error
	}{
		{"missing host", Config{}, `{}`, `{}`, nil},
		{"NUL config", Config{Host: "h", ElephantPath: "x\x00y"}, `{}`, `{}`, nil},
		{"invalid payload", Config{Host: "h"}, `not json`, `{}`, nil},
		{"large payload", Config{Host: "h"}, `"` + strings.Repeat("x", MaxInputSize) + `"`, `{}`, nil},
		{"invalid response", Config{Host: "h"}, `{}`, `not json`, nil},
		{"multiple responses", Config{Host: "h"}, `{}`, `{} {}`, nil},
		{"large response", Config{Host: "h"}, `{}`, `"` + strings.Repeat("x", MaxResponseSize+1) + `"`, nil},
		{"transport failure", Config{Host: "h"}, `{}`, `{}`, errors.New("SSH unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Backend{Executor: executorFunc(func(_ context.Context, _ string, _ []string, _ io.Reader, out, errout io.Writer) error {
				_, err := io.WriteString(out, tc.response)
				if err != nil {
					return err
				}
				return tc.runErr
			})}
			if _, err := b.Exchange(context.Background(), tc.config, []byte(tc.payload)); err == nil {
				t.Fatal("accepted invalid exchange")
			}
		})
	}
}

func TestCanceledExchangeDoesNotExecute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := Backend{Executor: executorFunc(func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error {
		t.Fatal("executed canceled request")
		return nil
	})}
	if _, err := b.Exchange(ctx, Config{Host: "h"}, []byte(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}
