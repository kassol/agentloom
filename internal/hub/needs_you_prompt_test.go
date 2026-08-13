package hub

import (
	"strings"
	"testing"
)

func TestBuiltinAgentPromptDefinesNeedsYouBlockedTurnAndFieldContract(t *testing.T) {
	for _, want := range []string{
		"必须先成功创建 required Needs You，再结束 Turn",
		"不会从最终自然语言自动代建 Needs You",
		"`question`：简短、单行的标题",
		"`context`：较长的 Markdown",
		"`blockedWork`：简短纯文本",
		"不要写字面量 `\\n`",
	} {
		if !strings.Contains(builtinLoomAgentPrompt, want) {
			t.Fatalf("builtin Agent prompt missing contract %q", want)
		}
	}
	if !strings.Contains(builtinLoomAgentPrompt, "--context \"## 背景\n\n已验证的构建") ||
		strings.Contains(builtinLoomAgentPrompt, "--context \"## 背景\\n") {
		t.Fatal("builtin Agent prompt example must pass real newlines")
	}
}
