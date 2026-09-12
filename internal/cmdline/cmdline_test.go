package cmdline

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractJSONFlag(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantEnabled bool
		wantRest    []string
	}{
		{name: "absent", args: []string{"ping"}, wantRest: []string{"ping"}},
		{name: "leading bare flag", args: []string{"--json", "ping"}, wantEnabled: true, wantRest: []string{"ping"}},
		{name: "trailing bare flag", args: []string{"ping", "--json"}, wantEnabled: true, wantRest: []string{"ping"}},
		{name: "explicit true", args: []string{"--json=true", "ping"}, wantEnabled: true, wantRest: []string{"ping"}},
		{name: "explicit false", args: []string{"--json=false", "ping"}, wantRest: []string{"ping"}},
		{name: "preserves other flags", args: []string{"cfg", "node", "show", "--json"}, wantEnabled: true, wantRest: []string{"cfg", "node", "show"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enabled, rest, err := ExtractJSONFlag(tt.args)
			if err != nil {
				t.Fatalf("ExtractJSONFlag returned error: %v", err)
			}
			if enabled != tt.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, tt.wantEnabled)
			}
			if !reflect.DeepEqual(rest, tt.wantRest) {
				t.Fatalf("rest = %v, want %v", rest, tt.wantRest)
			}
		})
	}
}

func TestExtractJSONFlag_InvalidValue(t *testing.T) {
	if _, _, err := ExtractJSONFlag([]string{"--json=maybe", "ping"}); err == nil {
		t.Fatal("ExtractJSONFlag returned nil error, want error for invalid bool")
	}
}

// --config applies to the whole command line, so a tool flag of the same name can
// never be passed; that is why the destination flag is --dest.
func TestASecondGlobalConfigFlagIsRefused(t *testing.T) {
	_, _, err := ExtractConfigFlag([]string{"--config", "/gw/config.yaml", "cfg", "node", "split", "--config", "/node/config.yaml"}, "/default")
	if err == nil {
		t.Fatal("a second --config was accepted; it silently becomes the target of the whole command")
	}
	if !strings.Contains(err.Error(), "--dest") {
		t.Errorf("the refusal does not name the flag to use instead: %v", err)
	}
}

func TestIsCommandSeparatesDaemonFlagsFromSubcommands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-node-listen", "127.0.0.1:8787"}, false},
		{[]string{"--version"}, false},
		{[]string{"ping"}, true},
		{[]string{"cfg", "launchd"}, true},
	} {
		if got := IsCommand(tc.args); got != tc.want {
			t.Errorf("IsCommand(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
