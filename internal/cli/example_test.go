package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func executeCommand(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()

	root := NewRootCmd(BuildInfo{Version: "test", Commit: "none", Date: "unknown"})

	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)
	return buf.String(), err
}

func TestExampleCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{
			name: "default greeting",
			args: []string{"example"},
			want: "Hello, World!\n",
		},
		{
			name: "positional message",
			args: []string{"example", "Howdy"},
			want: "Howdy, World!\n",
		},
		{
			name: "name and count flags",
			args: []string{"example", "--name", "Gopher", "--count", "2"},
			want: "Hello, Gopher!\nHello, Gopher!\n",
		},
		{
			name:    "too many args",
			args:    []string{"example", "one", "two"},
			wantErr: true,
		},
		{
			name:    "unknown flag",
			args:    []string{"example", "--bogus"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := executeCommand(t, context.Background(), tt.args...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExampleCommandCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := executeCommand(t, ctx, "example")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
}

func TestFlagsDoNotLeakBetweenRuns(t *testing.T) {
	if _, err := executeCommand(t, context.Background(), "example", "--name", "Gopher", "--count", "3"); err != nil {
		t.Fatal(err)
	}

	got, err := executeCommand(t, context.Background(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Hello, World!\n" {
		t.Fatalf("second run saw state from the first: %q", got)
	}
}
