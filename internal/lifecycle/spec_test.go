package lifecycle

import (
	"reflect"
	"testing"
)

func TestReadSpec(t *testing.T) {
	path := writeProject(t, map[string]string{"agent/spec.json": fixtureSpec})
	info, err := ReadSpec(path + "/agent/spec.json")
	if err != nil {
		t.Fatal(err)
	}
	// FALLBACK_MODEL (nested object), TELEGRAM_ALLOWED_USERS (config value),
	// PROVIDER_KEY (inside a longer string) — deduped and sorted.
	wantRefs := []string{"FALLBACK_MODEL", "PROVIDER_KEY", "TELEGRAM_ALLOWED_USERS"}
	if !reflect.DeepEqual(info.EnvRefs, wantRefs) {
		t.Errorf("EnvRefs = %v, want %v", info.EnvRefs, wantRefs)
	}
	wantIfEnv := []string{"TELEGRAM_ALLOWED_USERS"}
	if !reflect.DeepEqual(info.IfEnvNames, wantIfEnv) {
		t.Errorf("IfEnvNames = %v, want %v", info.IfEnvNames, wantIfEnv)
	}
	if info.AuthChoice != "zai-coding-global" {
		t.Errorf("AuthChoice = %q, want zai-coding-global", info.AuthChoice)
	}
}

func TestReadSpecNoZAIAuth(t *testing.T) {
	spec := `{"setup": {"auth_choice": "anthropic"}, "config": []}`
	path := writeProject(t, map[string]string{"agent/spec.json": spec})
	info, err := ReadSpec(path + "/agent/spec.json")
	if err != nil {
		t.Fatal(err)
	}
	if info.RequiresZAIKey() {
		t.Error("RequiresZAIKey must be false for non-zai auth_choice")
	}
}

func TestRequiredEnvVars(t *testing.T) {
	info := SpecInfo{
		EnvRefs:    []string{"ALWAYS", "GUARDED", "ZAI_API_KEY"},
		IfEnvNames: []string{"GUARDED"},
		AuthChoice: "zai-coding-global",
	}
	// GUARDED drops out (if_env = optional by contract); ZAI_API_KEY is
	// already a ref, so no duplicate is appended.
	want := []string{"ALWAYS", "ZAI_API_KEY"}
	if got := RequiredEnvVars(info); !reflect.DeepEqual(got, want) {
		t.Errorf("RequiredEnvVars = %v, want %v", got, want)
	}

	noZai := SpecInfo{EnvRefs: []string{"ALWAYS"}, AuthChoice: "local"}
	if got := RequiredEnvVars(noZai); !reflect.DeepEqual(got, []string{"ALWAYS"}) {
		t.Errorf("RequiredEnvVars(no zai) = %v, want [ALWAYS]", got)
	}
}
