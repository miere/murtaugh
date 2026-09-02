package auth

import (
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestLookupKnownProfiles(t *testing.T) {
	for _, name := range []string{"gcloud", "gcloud-adc"} {
		p, ok := Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) missed", name)
		}
		if p.Name != name {
			t.Fatalf("Name = %q, want %q", p.Name, name)
		}
		if !p.NeedsCode {
			t.Fatalf("%s should be a code flow", name)
		}
		if p.Command != "gcloud" {
			t.Fatalf("%s Command = %q, want gcloud", name, p.Command)
		}
		// --no-launch-browser is what makes gcloud print a URL and wait for a
		// code instead of trying to open a browser on a headless host.
		if !slices.Contains(p.Args, "--no-launch-browser") {
			t.Fatalf("%s args must pin --no-launch-browser, got %v", name, p.Args)
		}
	}
}

// gcloud-adc must run non-interactively. With GOOGLE_APPLICATION_CREDENTIALS
// already set, `gcloud auth application-default login` asks to confirm the
// overwrite before it prints anything; unanswered, it dies without a consent URL
// and the flow stalls until waitForURL gives up, posting no card at all.
func TestGcloudADCSuppressesPrompts(t *testing.T) {
	p, ok := Lookup("gcloud-adc")
	if !ok {
		t.Fatal("Lookup(\"gcloud-adc\") missed")
	}
	if !slices.Contains(p.Args, "--quiet") {
		t.Fatalf("gcloud-adc args must pin --quiet, got %v", p.Args)
	}
}

// aws is named in the spec but not shipped yet. It must fail as an unknown
// profile rather than resolve to something that cannot complete.
func TestAWSProfileIsNotYetAvailable(t *testing.T) {
	if _, ok := Lookup("aws"); ok {
		t.Fatal("aws resolved; it is not implemented yet")
	}
	if _, err := Resolve("aws", "", false); err == nil {
		t.Fatal("expected Resolve to reject aws")
	}
}

func TestNamesIncludesCustomAndIsSorted(t *testing.T) {
	names := Names()
	if !slices.Contains(names, CustomProfileName) {
		t.Fatalf("Names missing %q: %v", CustomProfileName, names)
	}
	if !slices.IsSorted(names) {
		t.Fatalf("Names not sorted: %v", names)
	}
}

func TestCustomBuildsSingleShellCommand(t *testing.T) {
	p, err := Custom("my-cli login --headless", false)
	if err != nil {
		t.Fatalf("Custom: %v", err)
	}
	if p.NeedsCode {
		t.Fatal("needsCode should have been carried through as false")
	}
	if p.Command != "sh" || len(p.Args) != 2 || p.Args[0] != "-c" {
		t.Fatalf("expected sh -c form, got %s %v", p.Command, p.Args)
	}
	// The whole command line must stay in ONE argv slot: splitting it here
	// would silently change the meaning of quoted arguments.
	if p.Args[1] != "my-cli login --headless" {
		t.Fatalf("command line was altered: %q", p.Args[1])
	}
}

func TestCustomRequiresACommand(t *testing.T) {
	if _, err := Custom("   ", false); err == nil {
		t.Fatal("expected an error for an empty custom command")
	}
	if _, err := Resolve(CustomProfileName, "", false); err == nil {
		t.Fatal("expected Resolve to reject custom with no command")
	}
}

// Passing a command with a built-in profile is a mistake worth reporting: the
// caller believes it will run, and it would not.
func TestResolveRejectsCommandOnBuiltin(t *testing.T) {
	_, err := Resolve("gcloud", "some-other-command", false)
	if err == nil {
		t.Fatal("expected an error when a command accompanies a built-in profile")
	}
	if !strings.Contains(err.Error(), CustomProfileName) {
		t.Fatalf("error should point at the custom profile, got: %v", err)
	}
}

func TestResolveRequiresAProfile(t *testing.T) {
	if _, err := Resolve("  ", "", false); err == nil {
		t.Fatal("expected an error for a missing profile")
	}
}

func TestResolveCarriesNeedsCodeForCustom(t *testing.T) {
	p, err := Resolve(CustomProfileName, "do-auth", true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !p.NeedsCode {
		t.Fatal("needsCode was dropped")
	}
}

func TestExtractURLFromGcloudOutput(t *testing.T) {
	p, _ := Lookup("gcloud")
	const line = "Go to the following link in your browser: https://accounts.google.com/o/oauth2/auth?response_type=code&client_id=x"

	got, ok := p.ExtractURL(line)
	if !ok {
		t.Fatal("no URL extracted")
	}
	if !strings.HasPrefix(got, "https://accounts.google.com/o/oauth2/auth?") {
		t.Fatalf("URL = %q", got)
	}
}

// gcloud's output also carries documentation links; the pattern must not offer
// one of those as the thing to click.
func TestExtractURLIgnoresUnrelatedLinks(t *testing.T) {
	p, _ := Lookup("gcloud")
	if _, ok := p.ExtractURL("See https://cloud.google.com/sdk/docs for help"); ok {
		t.Fatal("a docs link was mistaken for the verification URL")
	}
}

func TestExtractURLTrimsTrailingPunctuation(t *testing.T) {
	p, _ := Lookup("gcloud")
	for _, line := range []string{
		"visit https://accounts.google.com/o/oauth2/auth?a=1.",
		`visit "https://accounts.google.com/o/oauth2/auth?a=1"`,
		"visit (https://accounts.google.com/o/oauth2/auth?a=1)",
	} {
		got, ok := p.ExtractURL(line)
		if !ok {
			t.Fatalf("no URL extracted from %q", line)
		}
		if got != "https://accounts.google.com/o/oauth2/auth?a=1" {
			t.Fatalf("from %q got %q", line, got)
		}
	}
}

func TestExtractURLNoMatch(t *testing.T) {
	p, _ := Lookup("gcloud")
	if _, ok := p.ExtractURL("nothing to see here"); ok {
		t.Fatal("matched a line with no URL")
	}
}

// A custom flow's output shape is unknown, so it takes any https URL — but
// never a plaintext one.
func TestCustomExtractsAnyHTTPSButNotHTTP(t *testing.T) {
	p, _ := Custom("x", false)
	if _, ok := p.ExtractURL("open http://insecure.example/auth"); ok {
		t.Fatal("a plaintext URL was offered as an auth link")
	}
	got, ok := p.ExtractURL("open https://vendor.example/device?code=1")
	if !ok || got != "https://vendor.example/device?code=1" {
		t.Fatalf("ExtractURL = %q, %v", got, ok)
	}
}

func TestSpecMirrorsProfileArgv(t *testing.T) {
	p, _ := Lookup("gcloud-adc")
	spec := p.Spec()
	if spec.Command != p.Command || !slices.Equal(spec.Args, p.Args) {
		t.Fatalf("Spec drifted from the profile: %+v vs %s %v", spec, p.Command, p.Args)
	}
}

func TestSucceededTracksExitStatus(t *testing.T) {
	if !Succeeded(nil) {
		t.Fatal("a clean exit should count as success")
	}
	if Succeeded(&exec.ExitError{}) {
		t.Fatal("a non-zero exit should not count as success")
	}
	if Succeeded(errors.New("cancelled")) {
		t.Fatal("a cancelled run should not count as success")
	}
}
