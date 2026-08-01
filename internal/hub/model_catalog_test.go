package hub

import "testing"

func TestValidateModelEffortUsesCatalogCapabilities(t *testing.T) {
	if err := validateModelEffort("deepseek", "deepseek-v4-flash", "max"); err != nil {
		t.Fatalf("DeepSeek max effort: %v", err)
	}
	if err := validateModelEffort("deepseek", "deepseek-v4-flash", "medium"); err == nil {
		t.Fatal("DeepSeek medium effort should be rejected by catalog metadata")
	}
	if err := validateModelEffort("", "gpt-5.6-sol", "ultra"); err != nil {
		t.Fatalf("OpenAI ultra effort: %v", err)
	}
}
