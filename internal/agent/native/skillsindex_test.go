package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

// writeSkill creates <skillsDir>/<name>/SKILL.md with optional frontmatter.
func writeSkill(t *testing.T, skillsDir, name, frontmatterName, desc, body string) {
	t.Helper()
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var content string
	if frontmatterName != "" || desc != "" {
		content = "---\nname: " + frontmatterName + "\ndescription: " + desc + "\n---\n" + body
	} else {
		content = body
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRenderSkillsIndex(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "Do alpha things", "# Alpha\nbody")
	writeSkill(t, dir, "bravo", "", "", "# Bravo Heading\nbody") // no frontmatter → heading is the summary
	// A non-skill directory (no SKILL.md) must be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "not-a-skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	idx := renderSkillsIndex(dir)
	if !strings.Contains(idx, "- alpha: Do alpha things") {
		t.Errorf("index missing alpha entry:\n%s", idx)
	}
	if !strings.Contains(idx, "- bravo: Bravo Heading") {
		t.Errorf("index missing bravo (heading-as-summary) entry:\n%s", idx)
	}
	if strings.Contains(idx, "not-a-skill") {
		t.Errorf("index included a non-skill directory:\n%s", idx)
	}
}

func TestRenderSkillsIndex_EmptyWhenNoSkills(t *testing.T) {
	if got := renderSkillsIndex(t.TempDir()); got != "" {
		t.Errorf("expected empty index for an empty dir, got %q", got)
	}
	if got := renderSkillsIndex(filepath.Join(t.TempDir(), "does-not-exist")); got != "" {
		t.Errorf("expected empty index for a missing dir, got %q", got)
	}
}

func TestBuild_SkillsIndexGatedByAllowlist(t *testing.T) {
	t.Setenv("TEST_SKILLS_KEY", "x")
	base := t.TempDir()
	skillsDir := filepath.Join(base, ".agents", "skills")
	writeSkill(t, skillsDir, "alpha", "alpha", "Do alpha things", "# Alpha")

	profile := config.AgentProfile{
		Provider:  "gemini",
		Model:     "gemini-2.5-pro",
		APIKeyEnv: "TEST_SKILLS_KEY",
	}

	// Without "skills" in the allowlist: nothing advertised.
	cNo, err := Build(profile, BuildDeps{BaseDir: base})
	if err != nil {
		t.Fatalf("Build (no skills): %v", err)
	}
	if cNo.skillsIndex != "" {
		t.Errorf("skills index populated without the skills tool allowlisted: %q", cNo.skillsIndex)
	}

	// With "skills" allowlisted: the index is populated and lands in the system.
	profile.Tools = []string{"skills"}
	cYes, err := Build(profile, BuildDeps{BaseDir: base})
	if err != nil {
		t.Fatalf("Build (with skills): %v", err)
	}
	if !strings.Contains(cYes.skillsIndex, "- alpha: Do alpha things") {
		t.Errorf("skills index not populated: %q", cYes.skillsIndex)
	}
	system := BuildSystemPrompt(cYes.systemPrompt, cYes.skillsIndex)
	if !strings.Contains(system, "<skills>") || !strings.Contains(system, "alpha") {
		t.Errorf("skills index not folded into the system prompt:\n%s", system)
	}
}
